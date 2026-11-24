package scheduling

import (
	"fmt"
	"slices"
	"testing"

	"agones.dev/agones/pkg/apis"
	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"pgregory.net/rapid"

	litev1 "github.com/ponyo877/agones-lite/pkg/apis/lite/v1"
)

// These tests state what must hold wherever Place puts a GameServer and
// let rapid look for nodes and neighbours that break it. A failure is
// printed as the smallest such world.

// someNodes draws up to four nodes that are short of everything: a few
// host ports, room for a few GameServers, a core or two. Some are out of
// reach, cordoned or draining.
func someNodes(t *rapid.T) []*litev1.Node {
	nodes := make([]*litev1.Node, rapid.IntRange(0, 4).Draw(t, "nodes"))
	for i := range nodes {
		nodes[i] = node(fmt.Sprintf("n%d", i+1), func(n *litev1.Node) {
			n.Status.Phase = rapid.SampledFrom([]litev1.NodePhase{litev1.NodeReady, litev1.NodeReady, litev1.NodeReady, litev1.NodeDisconnected}).Draw(t, "phase")
			n.Spec.Unschedulable = rapid.IntRange(0, 5).Draw(t, "cordoned") == 0
			n.Spec.Drain = rapid.IntRange(0, 5).Draw(t, "draining") == 0
			n.Status.MaxGameServers = rapid.Int32Range(0, 3).Draw(t, "maxGameServers")
			n.Status.Capacity.CPU = someOf(t, "cpu", "0", "1", "2")
			n.Status.Capacity.Memory = someOf(t, "memory", "0", "1Gi")
			n.Status.PortRange = litev1.PortRange{Min: 7000 + rapid.Int32Range(0, 2).Draw(t, "minPort"), Max: 7000 + rapid.Int32Range(0, 4).Draw(t, "maxPort")}
		})
	}
	return nodes
}

func someOf(t *rapid.T, label string, amounts ...string) resource.Quantity {
	return resource.MustParse(rapid.SampledFrom(amounts).Draw(t, label))
}

// aGameServer draws a GameServer that asks for a little of everything, or
// for nothing. Its Static ports are distinct, as admission guarantees.
func aGameServer(t *rapid.T, name string) *agonesv1.GameServer {
	gs := server(name, "", "")
	gs.Spec.Scheduling = rapid.SampledFrom([]apis.SchedulingStrategy{apis.Packed, apis.Distributed}).Draw(t, "scheduling")
	gs.Spec.Template.Spec.Containers[0].Resources.Requests = corev1.ResourceList{
		corev1.ResourceCPU:    someOf(t, "cpuAsked", "0", "500m", "1"),
		corev1.ResourceMemory: someOf(t, "memoryAsked", "0", "512Mi"),
	}
	gs.Spec.Ports = nil
	taken := map[int32]bool{}
	for i := range rapid.IntRange(0, 3).Draw(t, "ports") {
		port := agonesv1.GameServerPort{Name: fmt.Sprint("port", i), ContainerPort: 7654}
		port.PortPolicy = rapid.SampledFrom([]agonesv1.PortPolicy{agonesv1.Dynamic, agonesv1.Passthrough, agonesv1.Static, agonesv1.None}).Draw(t, "policy")
		port.Protocol = rapid.SampledFrom([]corev1.Protocol{corev1.ProtocolUDP, corev1.ProtocolTCP, agonesv1.ProtocolTCPUDP}).Draw(t, "protocol")
		if port.PortPolicy == agonesv1.Static {
			port.HostPort = rapid.Int32Range(7000, 7005).Filter(func(p int32) bool { return !taken[p] }).Draw(t, "hostPort")
			taken[port.HostPort] = true
		}
		gs.Spec.Ports = append(gs.Spec.Ports, port)
	}
	return gs
}

// someNeighbours draws the GameServers that are there already: placed by
// Place itself on one of the nodes, in the order they came. Some sit on a
// node that is gone since, and some have no node yet.
func someNeighbours(t *rapid.T, nodes []*litev1.Node) []*agonesv1.GameServer {
	var servers []*agonesv1.GameServer
	for i := range rapid.IntRange(0, 8).Draw(t, "neighbours") {
		gs := aGameServer(t, fmt.Sprintf("gs-%d", i))
		if placed, ok := Place(gs, everyNodeOpen(nodes), servers); ok && rapid.IntRange(0, 5).Draw(t, "unplaced") > 0 {
			gs = placed
		}
		if rapid.IntRange(0, 7).Draw(t, "nodeGone") == 0 {
			gs.Status.NodeName = "gone"
		}
		servers = append(servers, gs)
	}
	return servers
}

// everyNodeOpen returns the nodes as they were before any of them was
// cordoned, drained or lost: full nodes come from the time they were open.
func everyNodeOpen(nodes []*litev1.Node) []*litev1.Node {
	open := make([]*litev1.Node, len(nodes))
	for i, n := range nodes {
		open[i] = n.DeepCopy()
		open[i].Status.Phase, open[i].Spec = litev1.NodeReady, litev1.NodeSpec{}
	}
	return open
}

func residentsOf(node *litev1.Node, servers []*agonesv1.GameServer) []*agonesv1.GameServer {
	var residents []*agonesv1.GameServer
	for _, gs := range servers {
		if gs.Status.NodeName == node.Name {
			residents = append(residents, gs)
		}
	}
	return residents
}

// canTake says whether node has everything gs needs, one question at a time.
func canTake(node *litev1.Node, gs *agonesv1.GameServer, residents []*agonesv1.GameServer) bool {
	open := node.Status.Phase == litev1.NodeReady && !node.Spec.Unschedulable && !node.Spec.Drain
	return open && hasASlot(node, residents) && hasTheResources(node, gs, residents) && hasThePorts(node, gs, residents)
}

func hasASlot(node *litev1.Node, residents []*agonesv1.GameServer) bool {
	return node.Status.MaxGameServers == 0 || int32(len(residents)) < node.Status.MaxGameServers
}

func hasTheResources(node *litev1.Node, gs *agonesv1.GameServer, residents []*agonesv1.GameServer) bool {
	var cpu, memory int64 // in thousandths of a core, and in bytes
	for _, each := range append(slices.Clone(residents), gs) {
		asked := each.Spec.Template.Spec.Containers[0].Resources.Requests
		cpu += asked.Cpu().MilliValue()
		memory += asked.Memory().Value()
	}
	limit := node.Status.Capacity
	return (limit.CPU.IsZero() || cpu <= limit.CPU.MilliValue()) && (limit.Memory.IsZero() || memory <= limit.Memory.Value())
}

func hasThePorts(node *litev1.Node, gs *agonesv1.GameServer, residents []*agonesv1.GameServer) bool {
	busy := hostPortsOf(residents)
	needed := 0
	for _, port := range gs.Spec.Ports {
		switch port.PortPolicy {
		case agonesv1.Static:
			if busy[port.HostPort] {
				return false
			}
			busy[port.HostPort] = true // its own Static port is not free for its Dynamic ones
		case agonesv1.Dynamic, agonesv1.Passthrough:
			needed++
		}
	}
	free := 0
	for port := node.Status.PortRange.Min; port <= node.Status.PortRange.Max; port++ {
		if !busy[port] {
			free++
		}
	}
	return free >= needed
}

func hostPortsOf(servers []*agonesv1.GameServer) map[int32]bool {
	busy := map[int32]bool{}
	for _, gs := range servers {
		for _, port := range gs.Spec.Ports {
			if port.HostPort != 0 {
				busy[port.HostPort] = true
			}
		}
	}
	return busy
}

func nodeNamed(nodes []*litev1.Node, name string) *litev1.Node {
	i := slices.IndexFunc(nodes, func(n *litev1.Node) bool { return n.Name == name })
	if i < 0 {
		return nil
	}
	return nodes[i]
}

// A GameServer that is placed is started, and players are sent to it. So
// the node must be one that takes GameServers now, with a slot, the CPU and
// the memory for this one; and the host ports it is given must be nobody
// else's and, where the node chose them, from the node's own range.
func TestPlaceOnlyUsesANodeThatCanTakeTheGameServer(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		nodes := someNodes(t)
		servers, gs := someNeighbours(t, nodes), aGameServer(t, "new")

		placed, ok := Place(gs, nodes, servers)

		if !ok {
			return
		}
		node := nodeNamed(nodes, placed.Status.NodeName)
		require.NotNil(t, node, "placed on %q, which is no node", placed.Status.NodeName)
		residents := residentsOf(node, servers)
		assert.True(t, canTake(node, gs, residents), "%s cannot take it", node.Name)
		busy := hostPortsOf(residents)
		for _, port := range placed.Spec.Ports {
			inRange := node.Status.PortRange.Min <= port.HostPort && port.HostPort <= node.Status.PortRange.Max
			assert.False(t, busy[port.HostPort] && port.PortPolicy != agonesv1.None, "port %d is a neighbour's", port.HostPort)
			assert.True(t, inRange || port.PortPolicy == agonesv1.Static || port.PortPolicy == agonesv1.None, "port %d is outside %v", port.HostPort, node.Status.PortRange)
		}
	})
}

// A GameServer that could run somewhere must not be left waiting, and the
// node it gets must be the one its strategy prefers among those that can
// take it: Packed fills the fullest, so that others can empty and go;
// Distributed fills the emptiest, so that losing a node costs little.
func TestPlaceFindsTheNodeTheStrategyPrefersWheneverOneCanTakeIt(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		nodes := someNodes(t)
		servers, gs := someNeighbours(t, nodes), aGameServer(t, "new")
		able := nodesThatCanTake(gs, nodes, servers)

		placed, ok := Place(gs, nodes, servers)

		require.Equal(t, len(able) > 0, ok, "%d nodes can take it", len(able))
		if !ok {
			return
		}
		fullness := func(n *litev1.Node) int { return len(residentsOf(n, servers)) }
		chosen := fullness(nodeNamed(nodes, placed.Status.NodeName))
		for _, other := range able {
			assert.False(t, preferred(gs.Spec.Scheduling, fullness(other), chosen),
				"%s holds %d GameServers and the chosen node %d", other.Name, fullness(other), chosen)
		}
	})
}

func nodesThatCanTake(gs *agonesv1.GameServer, nodes []*litev1.Node, servers []*agonesv1.GameServer) []*litev1.Node {
	var able []*litev1.Node
	for _, node := range nodes {
		if canTake(node, gs, residentsOf(node, servers)) {
			able = append(able, node)
		}
	}
	return able
}

// preferred says whether the strategy likes a node with this many
// GameServers better than one with that many.
func preferred(strategy apis.SchedulingStrategy, this, that int) bool {
	if strategy == apis.Distributed {
		return this < that
	}
	return this > that
}

// What Place is given is the store's own view, shared with everything
// else that reads it. Place must only look, and answer with a copy.
func TestPlaceLeavesWhatItIsGivenAlone(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		nodes := someNodes(t)
		servers, gs := someNeighbours(t, nodes), aGameServer(t, "new")
		nodesBefore, serversBefore, gsBefore := copiesOf(nodes), copiesOf(servers), gs.DeepCopy()

		placed, _ := Place(gs, nodes, servers)

		assert.Equal(t, nodesBefore, nodes, "the nodes, and their order")
		assert.Equal(t, serversBefore, servers, "the GameServers, and their order")
		assert.Equal(t, gsBefore, gs, "the GameServer to place")
		assert.NotSame(t, gs, placed)
	})
}

func copiesOf[T interface{ DeepCopy() T }](items []T) []T {
	out := slices.Clone(items)
	for i, item := range out {
		out[i] = item.DeepCopy()
	}
	return out
}

// The store hands nodes and GameServers over in no particular order, and
// the same GameServer must land on the same node with the same ports.
func TestPlaceGivesTheSameAnswerForAnyOrderOfNodesAndGameServers(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		nodes := someNodes(t)
		servers, gs := someNeighbours(t, nodes), aGameServer(t, "new")
		want, wantOK := Place(gs, nodes, servers)

		placed, ok := Place(gs, rapid.Permutation(nodes).Draw(t, "nodesShuffled"), rapid.Permutation(servers).Draw(t, "serversShuffled"))

		assert.Equal(t, wantOK, ok)
		assert.Equal(t, want, placed)
	})
}

// A Room places many GameServers after one look at what the nodes hold.
// Each of them must get what Place gives it after a fresh look at
// everything, the ones placed before it included.
func TestARoomPlacesEachGameServerAsAFreshLookWould(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		nodes := someNodes(t)
		servers := someNeighbours(t, nodes)
		room := RoomOn(nodes, servers)

		for i := range rapid.IntRange(1, 6).Draw(t, "arrivals") {
			gs := aGameServer(t, fmt.Sprint("new-", i))
			want, wantOK := Place(gs, nodes, servers)

			placed, ok := room.Place(gs)

			require.Equal(t, wantOK, ok, "%s", gs.Name)
			require.Equal(t, want, placed, "%s", gs.Name)
			if ok {
				servers = append(servers, placed)
			}
		}
	})
}
