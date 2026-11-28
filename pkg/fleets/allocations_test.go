package fleets

import (
	"context"
	"testing"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ponyo877/scrim/pkg/control"
)

// A Fleet counts every time one of its GameServers goes into Allocated,
// and nothing else: not a GameServer that was Allocated already, nor one
// that is changed while it stays Allocated, nor one of no Fleet.
func TestAFleetCountsWhenItsGameServersAreAllocated(t *testing.T) {
	f := newFixture(t)
	set := f.addSet(current(f.fleet, "active", full(3)))
	f.addServer(set, agonesv1.GameServerStateAllocated) // before the controller looked
	a := f.addServer(set, agonesv1.GameServerStateReady)
	b := f.addServer(set, agonesv1.GameServerStateReady)
	alone := f.addServer(set, agonesv1.GameServerStateReady)
	f.change(alone, func(gs *agonesv1.GameServer) { delete(gs.Labels, agonesv1.FleetNameLabel) })
	s := f.stepper()

	f.change(a, allocate)
	f.change(a, func(gs *agonesv1.GameServer) { gs.Labels["match"] = "1" })
	f.change(b, allocate)
	f.change(alone, allocate)
	f.change(a, func(gs *agonesv1.GameServer) { gs.Status.State = agonesv1.GameServerStateReady })
	f.change(a, allocate)
	f.drain(s)
	assert.Equal(t, int64(3), f.allocations(), "a twice, b once")

	s.Resync()
	f.drain(s)
	assert.Equal(t, int64(3), f.allocations(), "what was added once is not added again")
}

func allocate(gs *agonesv1.GameServer) { gs.Status.State = agonesv1.GameServerStateAllocated }

func (f fixture) change(gs *agonesv1.GameServer, how func(*agonesv1.GameServer)) {
	f.t.Helper()
	require.NoError(f.t, f.servers.Change(gs.Namespace, gs.Name, func(current *agonesv1.GameServer) bool {
		how(current)
		return true
	}))
}

func (f fixture) allocations() int64 {
	f.t.Helper()
	fleet, ok := f.fleets.Get(f.fleet.Namespace, f.fleet.Name)
	require.True(f.t, ok)
	return fleet.Status.Allocations
}

// stepper puts the controller under a control.Stepper, which hears of the
// changes from then on.
func (f fixture) stepper() *control.Stepper {
	s := &control.Stepper{}
	s.Take(Kind, f.Controller)
	f.t.Cleanup(s.Close)
	return s
}

// drain runs whatever waits until nothing does. A controller that keeps
// finding work, each status it writes asking for another, fails the test.
func (f fixture) drain(s *control.Stepper) {
	f.t.Helper()
	for range 100 {
		pending := s.Pending()
		if len(pending) == 0 {
			return
		}
		require.NoError(f.t, s.Run(context.Background(), pending[0]))
	}
	f.t.Fatal("the controller never ran out of work")
}
