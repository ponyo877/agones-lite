package gameserverallocations

import (
	"testing"
	"time"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	allocationv1 "agones.dev/agones/pkg/apis/allocation/v1"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ponyo877/agones-lite/pkg/agones"
)

const (
	namespace = "default"

	ready     = agonesv1.GameServerStateReady
	allocated = agonesv1.GameServerStateAllocated
	reserved  = agonesv1.GameServerStateReserved
	scheduled = agonesv1.GameServerStateScheduled
	shutdown  = agonesv1.GameServerStateShutdown
	failed    = agonesv1.GameServerStateError

	ascending  = agonesv1.GameServerPriorityAscending
	descending = agonesv1.GameServerPriorityDescending
)

var noon = time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)

// option changes one thing about a GameServer built by server.
type option func(*agonesv1.GameServer)

func server(name string, state agonesv1.GameServerState, opts ...option) *agonesv1.GameServer {
	gs := &agonesv1.GameServer{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}}
	gs.Status.State = state
	for _, opt := range opts {
		opt(gs)
	}
	return gs
}

func on(node string) option {
	return func(gs *agonesv1.GameServer) { gs.Status.NodeName = node }
}

func in(namespace string) option {
	return func(gs *agonesv1.GameServer) { gs.Namespace = namespace }
}

func labelled(labels map[string]string) option {
	return func(gs *agonesv1.GameServer) { gs.Labels = labels }
}

func beingDeleted(gs *agonesv1.GameServer) {
	gs.DeletionTimestamp = &metav1.Time{Time: noon}
}

func counter(name string, count, capacity int64) option {
	return func(gs *agonesv1.GameServer) {
		if gs.Status.Counters == nil {
			gs.Status.Counters = map[string]agonesv1.CounterStatus{}
		}
		gs.Status.Counters[name] = agonesv1.CounterStatus{Count: count, Capacity: capacity}
	}
}

func list(name string, capacity int64, values ...string) option {
	return func(gs *agonesv1.GameServer) {
		if gs.Status.Lists == nil {
			gs.Status.Lists = map[string]agonesv1.ListStatus{}
		}
		gs.Status.Lists[name] = agonesv1.ListStatus{Capacity: capacity, Values: values}
	}
}

// request prepares a request the way the caller must before Find: defaults,
// validation, then the conversion of required and preferred into selectors.
func request(t *testing.T, spec allocationv1.GameServerAllocationSpec) *allocationv1.GameServerAllocation {
	t.Helper()
	require.NoError(t, agones.Setup())
	gsa := &allocationv1.GameServerAllocation{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace},
		Spec:       spec,
	}
	gsa.ApplyDefaults()
	require.Empty(t, gsa.Validate())
	gsa.Converter()
	return gsa
}

func withLabels(labels map[string]string) allocationv1.GameServerSelector {
	return allocationv1.GameServerSelector{LabelSelector: metav1.LabelSelector{MatchLabels: labels}}
}

func inState(state agonesv1.GameServerState) allocationv1.GameServerSelector {
	return allocationv1.GameServerSelector{GameServerState: &state}
}

func byCounter(name, order string) []agonesv1.Priority {
	return []agonesv1.Priority{{Type: agonesv1.GameServerPriorityCounter, Key: name, Order: order}}
}

func byList(name, order string) []agonesv1.Priority {
	return []agonesv1.Priority{{Type: agonesv1.GameServerPriorityList, Key: name, Order: order}}
}

func names(servers []*agonesv1.GameServer) []string {
	out := make([]string, len(servers))
	for i, gs := range servers {
		out[i] = gs.Name
	}
	return out
}
