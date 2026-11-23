package fleets

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ponyo877/agones-lite/pkg/store"
)

// fixture is a Controller on a store that holds one Fleet, driven by hand
// instead of Run.
type fixture struct {
	t *testing.T
	*Controller
	// fleet is the Fleet as stored: sets made from it are its own.
	fleet *agonesv1.Fleet
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	fleet := newFleet(t)
	s, err := store.Open(filepath.Join(t.TempDir(), "state.db"), store.Kinds())
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	c := NewController(s, nil)
	c.now = func() time.Time { return noon }
	stored, err := c.fleets.Create(fleet)
	require.NoError(t, err)
	return fixture{t: t, Controller: c, fleet: stored}
}

// addSet stores a set of the Fleet and returns it as stored.
func (f fixture) addSet(set *agonesv1.GameServerSet) *agonesv1.GameServerSet {
	f.t.Helper()
	stored, err := f.sets.Create(set)
	require.NoError(f.t, err)
	return stored
}

// addServer stores a GameServer of set in the given state.
func (f fixture) addServer(set *agonesv1.GameServerSet, state agonesv1.GameServerState) {
	f.t.Helper()
	gs := set.GameServer()
	gs.Status.State = state
	_, err := f.servers.Create(gs)
	require.NoError(f.t, err)
}

func (f fixture) sync() {
	f.t.Helper()
	require.NoError(f.t, f.Controller.sync(context.Background(), store.KeyOf(store.Fleets, f.fleet)))
}

func (f fixture) set(name string) *agonesv1.GameServerSet {
	f.t.Helper()
	set, ok := f.sets.Get(f.fleet.Namespace, name)
	require.True(f.t, ok, "the set should exist")
	return set
}

func TestOldSetWithoutGameServersIsDeleted(t *testing.T) {
	f := newFixture(t)
	f.addSet(current(f.fleet, "active", full(5)))
	f.addSet(old(f.fleet, "old", shape{}))

	f.sync()

	assert.NotNil(t, f.set("old").DeletionTimestamp)
	assert.Nil(t, f.set("active").DeletionTimestamp)
}

// A set's status is a pass behind its GameServers: it is written from what
// was there before the pass created any. Deleting a set takes its
// GameServers along, sessions and all, so the status is not enough to go by.
// Upstream goes by it.
func TestOldSetIsNotDeletedWhileItsStatusIsBehindItsGameServers(t *testing.T) {
	tests := map[string]agonesv1.GameServerState{
		"a GameServer that has just been created": agonesv1.GameServerStatePortAllocation,
		"a GameServer with a session on it":       agonesv1.GameServerStateAllocated,
	}
	for name, state := range tests {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.addSet(current(f.fleet, "active", full(5)))
			behind := f.addSet(old(f.fleet, "old", shape{})) // its status says: nothing here
			f.addServer(behind, state)

			f.sync()

			assert.Nil(t, f.set("old").DeletionTimestamp, "it goes once its status shows that it is empty")
		})
	}
}
