package store

import (
	"path/filepath"
	"testing"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func open(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(path, Kinds())
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func gameServer(name string) *agonesv1.GameServer {
	return &agonesv1.GameServer{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name}}
}

func TestCreateFillsInMetadata(t *testing.T) {
	servers := GameServersOf(open(t, filepath.Join(t.TempDir(), "state.db")))

	created, err := servers.Create(gameServer("a"))
	require.NoError(t, err)
	assert.NotEmpty(t, created.UID)
	assert.NotEmpty(t, created.ResourceVersion)
	assert.False(t, created.CreationTimestamp.IsZero())

	_, err = servers.Create(gameServer("a"))
	assert.ErrorIs(t, err, ErrExists)
}

func TestCreateWithGenerateName(t *testing.T) {
	servers := GameServersOf(open(t, filepath.Join(t.TempDir(), "state.db")))

	gs := &agonesv1.GameServer{ObjectMeta: metav1.ObjectMeta{Namespace: "default", GenerateName: "simple-"}}
	created, err := servers.Create(gs)
	require.NoError(t, err)
	assert.Regexp(t, `^simple-\w{5}$`, created.Name)

	_, err = servers.Create(&agonesv1.GameServer{})
	assert.Error(t, err, "neither name nor generateName")
}

func TestUpdateNeedsTheLatestVersion(t *testing.T) {
	servers := GameServersOf(open(t, filepath.Join(t.TempDir(), "state.db")))
	first, err := servers.Create(gameServer("a"))
	require.NoError(t, err)

	stale := first.DeepCopy()
	first.Status.State = agonesv1.GameServerStateReady
	second, err := servers.Update(first)
	require.NoError(t, err)
	assert.NotEqual(t, stale.ResourceVersion, second.ResourceVersion)

	stale.Status.State = agonesv1.GameServerStateShutdown
	_, err = servers.Update(stale)
	assert.ErrorIs(t, err, ErrConflict)

	_, err = servers.Update(gameServer("missing"))
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestReadsReturnPrivateCopies(t *testing.T) {
	servers := GameServersOf(open(t, filepath.Join(t.TempDir(), "state.db")))
	_, err := servers.Create(gameServer("a"))
	require.NoError(t, err)

	got, _ := servers.Get("default", "a")
	got.Status.State = agonesv1.GameServerStateError

	again, _ := servers.Get("default", "a")
	assert.Empty(t, again.Status.State, "changing a copy must not change the store")
}

func TestListFiltersAndSorts(t *testing.T) {
	servers := GameServersOf(open(t, filepath.Join(t.TempDir(), "state.db")))
	for _, name := range []string{"b", "a"} {
		_, err := servers.Create(gameServer(name))
		require.NoError(t, err)
	}
	other := gameServer("c")
	other.Namespace = "other"
	_, err := servers.Create(other)
	require.NoError(t, err)

	assert.Equal(t, []string{"a", "b"}, names(servers.List("default")))
	assert.Equal(t, []string{"a", "b", "c"}, names(servers.List("")))
}

func TestWatchSeesEveryChange(t *testing.T) {
	servers := GameServersOf(open(t, filepath.Join(t.TempDir(), "state.db")))
	events, cancel := servers.Watch()

	gs, err := servers.Create(gameServer("a"))
	require.NoError(t, err)
	_, err = servers.Update(gs)
	require.NoError(t, err)
	require.NoError(t, servers.Delete("default", "a"))

	assert.Equal(t, Added, (<-events).Type)
	assert.Equal(t, Modified, (<-events).Type)
	assert.Equal(t, Deleted, (<-events).Type)

	cancel()
	_, open := <-events
	assert.False(t, open, "cancel closes the channel")
}

func TestAWatcherThatFallsBehindIsClosed(t *testing.T) {
	servers := GameServersOf(open(t, filepath.Join(t.TempDir(), "state.db")))
	slow, cancelSlow := servers.Watch()
	quick, cancelQuick := servers.Watch()
	defer cancelQuick()

	gs, err := servers.Create(gameServer("a"))
	require.NoError(t, err)
	for range watchBuffer { // one change more than a watcher may have waiting
		<-quick
		gs, err = servers.Update(gs)
		require.NoError(t, err)
	}

	for range watchBuffer {
		_, open := <-slow
		require.True(t, open, "what it was told before falling behind is still there to read")
	}
	select {
	case _, open := <-slow:
		assert.False(t, open)
	default:
		assert.Fail(t, "the watcher is still open, with a hole in what it was told")
	}
	cancelSlow() // must cope with a channel that is closed already

	_, open := <-quick
	assert.True(t, open, "a watcher that keeps up is not disturbed")
}

func TestStateSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path, Kinds())
	require.NoError(t, err)
	created, err := GameServersOf(s).Create(gameServer("a"))
	require.NoError(t, err)
	require.NoError(t, GameServersOf(s).Delete("default", "a"))
	_, err = GameServersOf(s).Create(gameServer("b"))
	require.NoError(t, err)
	require.NoError(t, s.Close())

	servers := GameServersOf(open(t, path))
	assert.Equal(t, []string{"b"}, names(servers.List("")))

	// The revision counter must not restart, or old versions would look current.
	c, err := servers.Create(gameServer("c"))
	require.NoError(t, err)
	assert.Greater(t, c.ResourceVersion, created.ResourceVersion)
}

func names(servers []*agonesv1.GameServer) []string {
	var out []string
	for _, gs := range servers {
		out = append(out, gs.Name)
	}
	return out
}

func TestViewSharesWhatIsStoredAndUpdatesDoNotDisturbIt(t *testing.T) {
	servers := GameServersOf(open(t, filepath.Join(t.TempDir(), "state.db")))
	created, err := servers.Create(gameServer("a"))
	require.NoError(t, err)

	before := servers.View("default")
	require.Len(t, before, 1)

	created.Status.State = agonesv1.GameServerStateReady
	_, err = servers.Update(created)
	require.NoError(t, err)

	assert.Empty(t, before[0].Status.State, "a view taken earlier keeps showing the object as it was")
	assert.Equal(t, agonesv1.GameServerStateReady, servers.View("default")[0].Status.State)
	assert.Empty(t, servers.View("elsewhere"))
}

func TestListAndWatchStartsTheEventsWhereTheListEnds(t *testing.T) {
	servers := GameServersOf(open(t, filepath.Join(t.TempDir(), "state.db")))
	_, err := servers.Create(gameServer("before"))
	require.NoError(t, err)

	listed, events, cancel := servers.ListAndWatch("default")
	defer cancel()
	_, err = servers.Create(gameServer("after"))
	require.NoError(t, err)

	require.Len(t, listed, 1)
	assert.Equal(t, "before", listed[0].Name)
	change := <-events
	assert.Equal(t, Added, change.Type)
	assert.Equal(t, "after", change.Key.Name, "what was listed is not told again")
}
