package gameserverallocations

import (
	"testing"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	allocationv1 "agones.dev/agones/pkg/apis/allocation/v1"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
)

// stored is a GameServer the way the store holds it after an allocation.
func stored() *agonesv1.GameServer {
	gs := server("gs-1", allocated, on("node1"),
		labelled(map[string]string{"mode": "deathmatch"}),
		counter("rooms", 2, 10),
		list("players", 10, "alice"))
	gs.Annotations = map[string]string{lastAllocated: atNoon}
	gs.Status.Address = "203.0.113.10"
	gs.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeExternalIP, Address: "203.0.113.10"}}
	gs.Status.Ports = []agonesv1.GameServerStatusPort{{Name: "game", Port: 7000}}
	return gs
}

// Upstream: TestAllocatorAllocate and TestControllerAllocator check parts of
// this status; the fields are those allocateFromLocalCluster sets.
func TestAllocatedAnswersWithTheGameServer(t *testing.T) {
	gsa := request(t, allocationv1.GameServerAllocationSpec{
		Selectors: []allocationv1.GameServerSelector{withLabels(role)},
	})
	before := gsa.DeepCopy()

	answer := Allocated(gsa, stored())

	assert.Equal(t, "gs-1", answer.Name, "the answer is named after the GameServer")
	assert.Equal(t, allocationv1.GameServerAllocationStatus{
		State:          allocationv1.GameServerAllocationAllocated,
		GameServerName: "gs-1",
		Ports:          []agonesv1.GameServerStatusPort{{Name: "game", Port: 7000}},
		Address:        "203.0.113.10",
		Addresses:      []corev1.NodeAddress{{Type: corev1.NodeExternalIP, Address: "203.0.113.10"}},
		NodeName:       "node1",
		Source:         "local",
		Metadata: &allocationv1.GameServerMetadata{
			Labels:      map[string]string{"mode": "deathmatch"},
			Annotations: map[string]string{lastAllocated: atNoon},
		},
		Counters: map[string]agonesv1.CounterStatus{"rooms": {Count: 2, Capacity: 10}},
		Lists:    map[string]agonesv1.ListStatus{"players": {Values: []string{"alice"}, Capacity: 10}},
	}, answer.Status)
	assert.Equal(t, before.Spec, answer.Spec)
	assert.Equal(t, before, gsa, "the request itself is left alone")
}

func TestAllocatedSharesNothingWithTheGameServer(t *testing.T) {
	gsa := request(t, allocationv1.GameServerAllocationSpec{})
	gs := stored()
	before := gs.DeepCopy()

	answer := Allocated(gsa, gs)
	answer.Status.Metadata.Labels["mode"] = "changed"
	answer.Status.Counters["rooms"] = agonesv1.CounterStatus{}
	answer.Status.Lists["players"].Values[0] = "changed"
	answer.Status.Ports[0].Port = 1
	answer.Status.Addresses[0].Address = "changed"

	assert.Equal(t, before, gs)
}

func TestUnallocatedOnlySetsTheState(t *testing.T) {
	gsa := request(t, allocationv1.GameServerAllocationSpec{
		Selectors: []allocationv1.GameServerSelector{withLabels(role)},
	})
	before := gsa.DeepCopy()

	answer := Unallocated(gsa)

	assert.Equal(t, allocationv1.GameServerAllocationStatus{
		State: allocationv1.GameServerAllocationUnAllocated,
	}, answer.Status)
	assert.Empty(t, answer.Name)
	assert.Equal(t, before.Spec, answer.Spec)
	assert.Equal(t, before, gsa, "the request itself is left alone")
}
