package scheduling

import (
	"testing"

	"agones.dev/agones/pkg/apis"
	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	scrimv1 "github.com/ponyo877/scrim/pkg/apis/scrim/v1"
)

func node(name string, change ...func(*scrimv1.Node)) *scrimv1.Node {
	n := &scrimv1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: scrimv1.NodeStatus{
			Phase:     scrimv1.NodeReady,
			PortRange: scrimv1.PortRange{Min: 7000, Max: 7009},
		},
	}
	for _, fn := range change {
		fn(n)
	}
	return n
}

// server returns a GameServer with one dynamic port that asks for cpu cores.
func server(name, nodeName, cpu string) *agonesv1.GameServer {
	gs := &agonesv1.GameServer{ObjectMeta: metav1.ObjectMeta{Name: name}}
	gs.Spec.Ports = []agonesv1.GameServerPort{{Name: "game", PortPolicy: agonesv1.Dynamic, ContainerPort: 7654}}
	gs.Spec.Template.Spec.Containers = []corev1.Container{{Name: "game"}}
	if cpu != "" {
		gs.Spec.Template.Spec.Containers[0].Resources.Requests = corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse(cpu),
		}
	}
	gs.Status.NodeName = nodeName
	return gs
}

func TestPlaceChoosesByStrategy(t *testing.T) {
	nodes := []*scrimv1.Node{node("n1"), node("n2")}
	servers := []*agonesv1.GameServer{server("a", "n2", "")}

	packed := server("new", "", "")
	packed.Spec.Scheduling = apis.Packed
	placed, ok := Place(packed, nodes, servers)
	require.True(t, ok)
	assert.Equal(t, "n2", placed.Status.NodeName, "Packed prefers the fuller node")

	distributed := server("new", "", "")
	distributed.Spec.Scheduling = apis.Distributed
	placed, ok = Place(distributed, nodes, servers)
	require.True(t, ok)
	assert.Equal(t, "n1", placed.Status.NodeName, "Distributed prefers the emptier node")
}

func TestPlaceAssignsPortsAndLeavesTheInputAlone(t *testing.T) {
	gs := server("new", "", "")
	residents := []*agonesv1.GameServer{server("a", "n1", "")}
	residents[0].Spec.Ports[0].HostPort = 7000

	placed, ok := Place(gs, []*scrimv1.Node{node("n1")}, residents)

	require.True(t, ok)
	assert.Equal(t, int32(7001), placed.Spec.Ports[0].HostPort)
	assert.Zero(t, gs.Spec.Ports[0].HostPort)
	assert.Empty(t, gs.Status.NodeName)
}

func TestPlaceSkipsNodesThatCannotTakeIt(t *testing.T) {
	tests := map[string]struct {
		node      *scrimv1.Node
		residents []*agonesv1.GameServer
	}{
		"disconnected": {
			node: node("n1", func(n *scrimv1.Node) { n.Status.Phase = scrimv1.NodeDisconnected }),
		},
		"cordoned": {
			node: node("n1", func(n *scrimv1.Node) { n.Spec.Unschedulable = true }),
		},
		// Found by TestPlaceOnlyUsesANodeThatCanTakeTheGameServer.
		"draining, though not cordoned": {
			node: node("n1", func(n *scrimv1.Node) { n.Spec.Drain = true }),
		},
		"at the game server limit": {
			node:      node("n1", func(n *scrimv1.Node) { n.Status.MaxGameServers = 1 }),
			residents: []*agonesv1.GameServer{server("a", "n1", "")},
		},
		"out of cpu": {
			node:      node("n1", func(n *scrimv1.Node) { n.Status.Capacity.CPU = resource.MustParse("1") }),
			residents: []*agonesv1.GameServer{server("a", "n1", "800m")},
		},
		"out of ports": {
			node: node("n1", func(n *scrimv1.Node) { n.Status.PortRange = scrimv1.PortRange{Min: 7000, Max: 6999} }),
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			_, ok := Place(server("new", "", "500m"), []*scrimv1.Node{tc.node}, tc.residents)
			assert.False(t, ok)
		})
	}
}

// Kubernetes does not put two Pods that ask for the same host port on one node.
func TestPlaceSkipsANodeWhereTheStaticPortIsTaken(t *testing.T) {
	static := func(name, nodeName string) *agonesv1.GameServer {
		gs := server(name, nodeName, "")
		gs.Spec.Ports = []agonesv1.GameServerPort{{Name: "game", PortPolicy: agonesv1.Static, HostPort: 7777, ContainerPort: 7654}}
		return gs
	}
	resident := static("a", "n1")

	placed, ok := Place(static("new", ""), []*scrimv1.Node{node("n1"), node("n2")}, []*agonesv1.GameServer{resident})
	require.True(t, ok)
	assert.Equal(t, "n2", placed.Status.NodeName, "n1 is fuller, which Packed prefers, but its port 7777 is taken")

	_, ok = Place(static("new", ""), []*scrimv1.Node{node("n1")}, []*agonesv1.GameServer{resident})
	assert.False(t, ok, "it waits for a node where the port is free")
}

// A Static port may lie in the node's range, and is then not handed out again.
func TestPlaceKeepsDynamicPortsOffStaticOnes(t *testing.T) {
	resident := server("a", "n1", "")
	resident.Spec.Ports = []agonesv1.GameServerPort{{Name: "game", PortPolicy: agonesv1.Static, HostPort: 7000, ContainerPort: 7654}}
	both := server("new", "", "")
	both.Spec.Ports = append(both.Spec.Ports, agonesv1.GameServerPort{Name: "admin", PortPolicy: agonesv1.Static, HostPort: 7001, ContainerPort: 8080})

	placed, ok := Place(both, []*scrimv1.Node{node("n1")}, []*agonesv1.GameServer{resident})

	require.True(t, ok)
	assert.Equal(t, int32(7002), placed.Spec.Ports[0].HostPort, "7000 is a neighbour's and 7001 its own")
	assert.Equal(t, int32(7001), placed.Spec.Ports[1].HostPort)
}

func TestPlaceTreatsZeroCapacityAsUnlimited(t *testing.T) {
	_, ok := Place(server("new", "", "64"), []*scrimv1.Node{node("n1")}, nil)
	assert.True(t, ok)
}

// A node takes a GameServer only if it has every range the GameServer's
// ports name. One that names a range no node has is told so.
func TestAPortThatNamesARangeGoesToANodeWithThatRange(t *testing.T) {
	plain := node("n1")
	withGame := node("n2", func(n *scrimv1.Node) {
		n.Status.PortRanges = map[string]scrimv1.PortRange{"game": {Min: 5000, Max: 5009}}
	})
	gs := server("new", "", "")
	gs.Spec.Scheduling = apis.Packed
	gs.Spec.Ports[0].Range = "game"
	room := RoomOn([]*scrimv1.Node{plain, withGame}, []*agonesv1.GameServer{server("a", "n1", "")})

	placed, ok := room.Place(gs)

	require.True(t, ok)
	assert.Equal(t, "n2", placed.Status.NodeName, "though Packed prefers the fuller n1")
	assert.Equal(t, int32(5000), placed.Spec.Ports[0].HostPort)
	_, missing := room.Missing(gs)
	assert.False(t, missing)

	gs.Spec.Ports[0].Range = "query"
	_, ok = room.Place(gs)
	assert.False(t, ok)
	name, missing := room.Missing(gs)
	assert.True(t, missing)
	assert.Equal(t, "query", name)
}
