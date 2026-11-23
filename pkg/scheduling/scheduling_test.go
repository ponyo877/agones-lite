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

	litev1 "github.com/ponyo877/agones-lite/pkg/apis/lite/v1"
)

func node(name string, change ...func(*litev1.Node)) *litev1.Node {
	n := &litev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: litev1.NodeStatus{
			Phase:     litev1.NodeReady,
			PortRange: litev1.PortRange{Min: 7000, Max: 7009},
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
	nodes := []*litev1.Node{node("n1"), node("n2")}
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

	placed, ok := Place(gs, []*litev1.Node{node("n1")}, residents)

	require.True(t, ok)
	assert.Equal(t, int32(7001), placed.Spec.Ports[0].HostPort)
	assert.Zero(t, gs.Spec.Ports[0].HostPort)
	assert.Empty(t, gs.Status.NodeName)
}

func TestPlaceSkipsNodesThatCannotTakeIt(t *testing.T) {
	tests := map[string]struct {
		node      *litev1.Node
		residents []*agonesv1.GameServer
	}{
		"disconnected": {
			node: node("n1", func(n *litev1.Node) { n.Status.Phase = litev1.NodeDisconnected }),
		},
		"cordoned": {
			node: node("n1", func(n *litev1.Node) { n.Spec.Unschedulable = true }),
		},
		"at the game server limit": {
			node:      node("n1", func(n *litev1.Node) { n.Status.MaxGameServers = 1 }),
			residents: []*agonesv1.GameServer{server("a", "n1", "")},
		},
		"out of cpu": {
			node:      node("n1", func(n *litev1.Node) { n.Status.Capacity.CPU = resource.MustParse("1") }),
			residents: []*agonesv1.GameServer{server("a", "n1", "800m")},
		},
		"out of ports": {
			node: node("n1", func(n *litev1.Node) { n.Status.PortRange = litev1.PortRange{Min: 7000, Max: 6999} }),
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			_, ok := Place(server("new", "", "500m"), []*litev1.Node{tc.node}, tc.residents)
			assert.False(t, ok)
		})
	}
}

func TestPlaceTreatsZeroCapacityAsUnlimited(t *testing.T) {
	_, ok := Place(server("new", "", "64"), []*litev1.Node{node("n1")}, nil)
	assert.True(t, ok)
}
