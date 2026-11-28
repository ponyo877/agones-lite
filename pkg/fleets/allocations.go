package fleets

import (
	"sync"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"

	"github.com/ponyo877/scrim/pkg/store"
)

// allocations counts, for each Fleet, the GameServers of it that went into
// Allocated since its status last took them in. Status.Allocations is a
// running total, so these are added to it, not put in its place.
//
// A GameServer counts when it goes into Allocated, however it got there:
// from the allocator, or from its own SDK. One that was Allocated already
// and is changed again does not count twice.
//
// Upstream: allocTracker in pkg/fleets/controller.go. It is fed by an
// informer, which hands over each GameServer as it was before a change and
// after. A change from the store holds only the after, so the state before
// is kept here.
type allocations struct {
	servers store.Typed[*agonesv1.GameServer]
	mu      sync.Mutex
	states  map[store.Key]agonesv1.GameServerState
	waiting map[store.Key]int64 // by Fleet
}

func newAllocations(servers store.Typed[*agonesv1.GameServer]) *allocations {
	return &allocations{servers: servers, waiting: map[store.Key]int64{}}
}

// watch starts following the GameServers from the states they are in now.
// It is called again whenever the store gives up on the last subscription.
func (a *allocations) watch() (<-chan store.Event, func()) {
	servers, events, cancel := a.servers.ListAndWatch("")
	a.mu.Lock()
	defer a.mu.Unlock()
	a.states = make(map[store.Key]agonesv1.GameServerState, len(servers))
	for _, gs := range servers {
		a.states[store.KeyOf(store.GameServers, gs)] = gs.Status.State
	}
	return events, cancel
}

// seen takes in one change of a GameServer. When the change allocated it,
// its Fleet is named, to have its status written again.
func (a *allocations) seen(e store.Event) []store.Key {
	a.mu.Lock()
	defer a.mu.Unlock()
	before, known := a.states[e.Key]
	if e.Type == store.Deleted {
		delete(a.states, e.Key)
		return nil
	}
	gs := e.Object.(*agonesv1.GameServer)
	a.states[e.Key] = gs.Status.State
	fleet := gs.Labels[agonesv1.FleetNameLabel]
	// A GameServer seen for the first time was not allocated by this
	// change: it was created. Upstream counts on updates only, too.
	if !known || before == agonesv1.GameServerStateAllocated || gs.Status.State != agonesv1.GameServerStateAllocated || fleet == "" {
		return nil
	}
	key := store.Key{Kind: store.Fleets, Namespace: e.Key.Namespace, Name: fleet}
	a.waiting[key]++
	return []store.Key{key}
}

// count returns how many allocations of a Fleet wait to be added.
func (a *allocations) count(fleet store.Key) int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.waiting[fleet]
}

// added says that n of them are in the Fleet's status now. Those that
// came in since count was called still wait.
func (a *allocations) added(fleet store.Key, n int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.waiting[fleet] -= n
	if a.waiting[fleet] == 0 {
		delete(a.waiting, fleet)
	}
}

// forget drops what waits for a Fleet that is gone.
func (a *allocations) forget(fleet store.Key) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.waiting, fleet)
}
