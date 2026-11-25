package gameserversets

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ponyo877/scrim/pkg/agones"
	"github.com/ponyo877/scrim/pkg/gameservers"
	"github.com/ponyo877/scrim/pkg/store"
)

// fixture is a Controller on a store that holds one GameServerSet, driven
// by hand instead of Run.
type fixture struct {
	t *testing.T
	*Controller
	// set is the set as stored: GameServers made from it are its own.
	set *agonesv1.GameServerSet
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "state.db"), store.Kinds())
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// newFixture stores a set that wants replicas GameServers. removals is the
// store through which GameServers are removed: the fixture's own when nil.
func newFixture(t *testing.T, replicas int32, removals *store.Store) fixture {
	t.Helper()
	require.NoError(t, agones.Setup())
	s := openStore(t)
	if removals == nil {
		removals = s
	}
	c := NewController(s, gameservers.NewController(removals, nil), nil)
	c.now = func() time.Time { return noon }
	stored, err := c.sets.Create(setOf(replicas))
	require.NoError(t, err)
	return fixture{t: t, Controller: c, set: stored}
}

// addServer stores a GameServer of the set in the given state.
func (f fixture) addServer(name string, state agonesv1.GameServerState) *agonesv1.GameServer {
	f.t.Helper()
	gs := f.set.GameServer()
	gs.Name, gs.Status.State = name, state
	stored, err := f.servers.Create(gs)
	require.NoError(f.t, err)
	return stored
}

func (f fixture) sync() error {
	return f.Controller.sync(context.Background(), store.KeyOf(store.GameServerSets, f.set))
}

func (f fixture) status() agonesv1.GameServerSetStatus {
	f.t.Helper()
	set, ok := f.sets.Get(f.set.Namespace, f.set.Name)
	require.True(f.t, ok, "the set should exist")
	return set.Status
}

// The Fleet and the autoscaler steer by the status. Written from what was
// there before the pass, it showed a set that had just been filled as empty,
// and one that had just been emptied as full, until something else made the
// controller look again.
func TestTheStatusShowsWhatThePassItselfDid(t *testing.T) {
	t.Run("the GameServers it created are replicas", func(t *testing.T) {
		f := newFixture(t, 2, nil)

		require.NoError(t, f.sync())

		assert.Equal(t, int32(2), f.status().Replicas)
	})
	t.Run("the GameServers it told to shut down are none any more", func(t *testing.T) {
		f := newFixture(t, 0, nil)
		f.addServer("surplus", ready)

		require.NoError(t, f.sync())

		status := f.status()
		assert.Zero(t, status.Replicas+status.ReadyReplicas)
		assert.Equal(t, int32(1), status.ShutdownReplicas)
	})
}

// brokenStore returns a store that holds gs and can no longer be written
// to, like one whose disk is gone.
func brokenStore(t *testing.T, gs *agonesv1.GameServer) *store.Store {
	t.Helper()
	s := openStore(t)
	_, err := store.GameServersOf(s).Create(gs)
	require.NoError(t, err)
	require.NoError(t, s.Close())
	return s
}

// A pass that fails part way is tried again, and may fail again. Meanwhile
// sessions begin and end, and a status that waits for a pass without
// failures would tell the Fleet and the autoscaler less and less of it.
func TestTheStatusIsWrittenEvenWhenThePlanCouldNotBeCarriedOut(t *testing.T) {
	sick := server("sick", unhealthy)
	sick.Namespace = "default"
	f := newFixture(t, 2, brokenStore(t, sick))
	f.addServer("busy", allocated)
	f.addServer("sick", unhealthy)

	err := f.sync()

	require.Error(t, err, "the sick GameServer could not be removed, so the pass is tried again")
	want := agonesv1.GameServerSetStatus{Replicas: 3, AllocatedReplicas: 1}
	got := f.status()
	got.Counters, got.Lists = nil, nil
	assert.Equal(t, want, got, "the session, the sick GameServer that is still there, and the one made to replace it")
}
