package gameserversets

import (
	"time"

	"agones.dev/agones/pkg/apis"
	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	portAllocation = agonesv1.GameServerStatePortAllocation
	creating       = agonesv1.GameServerStateCreating
	requestReady   = agonesv1.GameServerStateRequestReady
	ready          = agonesv1.GameServerStateReady
	reserved       = agonesv1.GameServerStateReserved
	allocated      = agonesv1.GameServerStateAllocated
	shutdown       = agonesv1.GameServerStateShutdown
	unhealthy      = agonesv1.GameServerStateUnhealthy
	failed         = agonesv1.GameServerStateError
)

var noon = time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)

// option changes one thing about a GameServer built by server.
type option func(*agonesv1.GameServer)

func server(name string, state agonesv1.GameServerState, opts ...option) *agonesv1.GameServer {
	gs := &agonesv1.GameServer{ObjectMeta: metav1.ObjectMeta{Name: name}}
	gs.Status.State = state
	for _, opt := range opts {
		opt(gs)
	}
	return gs
}

// of returns one nameless GameServer for each state.
func of(states ...agonesv1.GameServerState) []*agonesv1.GameServer {
	servers := make([]*agonesv1.GameServer, len(states))
	for i, state := range states {
		servers[i] = server("", state)
	}
	return servers
}

// many returns n nameless GameServers that are all alike.
func many(n int, state agonesv1.GameServerState, opts ...option) []*agonesv1.GameServer {
	servers := make([]*agonesv1.GameServer, n)
	for i := range servers {
		servers[i] = server("", state, opts...)
	}
	return servers
}

func on(node string) option {
	return func(gs *agonesv1.GameServer) { gs.Status.NodeName = node }
}

func created(at time.Time) option {
	return func(gs *agonesv1.GameServer) { gs.CreationTimestamp = metav1.NewTime(at) }
}

func beingDeleted(gs *agonesv1.GameServer) {
	gs.DeletionTimestamp = &metav1.Time{Time: noon}
}

// erroredAt records when the GameServer failed, the way upstream writes it.
func erroredAt(stamp string) option {
	return func(gs *agonesv1.GameServer) {
		metav1.SetMetaDataAnnotation(&gs.ObjectMeta, agonesv1.GameServerErroredAtAnnotation, stamp)
	}
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

// setOf returns a Distributed GameServerSet that wants replicas GameServers.
func setOf(replicas int32) *agonesv1.GameServerSet {
	return &agonesv1.GameServerSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "set"},
		Spec:       agonesv1.GameServerSetSpec{Replicas: replicas, Scheduling: apis.Distributed},
	}
}

func names(servers []*agonesv1.GameServer) []string {
	out := make([]string, len(servers))
	for i, gs := range servers {
		out[i] = gs.Name
	}
	return out
}
