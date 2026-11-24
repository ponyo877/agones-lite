package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
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

// A Fleet named with 63 characters makes a GameServerSet, and that makes
// GameServers, each with five characters more. Kubernetes cuts the base so
// that the result still fits a label; so must we.
func TestAGeneratedNameIsCutToFitALabel(t *testing.T) {
	servers := GameServersOf(open(t, filepath.Join(t.TempDir(), "state.db")))
	long := strings.Repeat("f", 63) + "-"

	created, err := servers.Create(&agonesv1.GameServer{ObjectMeta: metav1.ObjectMeta{Namespace: "default", GenerateName: long}})

	require.NoError(t, err)
	assert.Regexp(t, `^f{58}[a-z0-9]{5}$`, created.Name)
	assert.Empty(t, validation.IsDNS1123Label(created.Name))
}

// JSON keeps a time to the second. A finer one in memory would differ from
// the file, and two GameServers made within one second would be in order
// until the next restart and tied after it.
func TestTimesAreTheSameBeforeAndAfterARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path, Kinds())
	require.NoError(t, err)
	_, err = GameServersOf(s).Create(gameServer("a"))
	require.NoError(t, err)
	require.NoError(t, GameServersOf(s).MarkDeleted("default", "a", time.Now()))
	before, _ := GameServersOf(s).Get("default", "a")
	require.NoError(t, s.Close())

	after, _ := GameServersOf(open(t, path)).Get("default", "a")

	assert.True(t, before.CreationTimestamp.Time.Equal(after.CreationTimestamp.Time), "created %s, read back as %s", before.CreationTimestamp.Time, after.CreationTimestamp.Time)
	assert.True(t, before.DeletionTimestamp.Time.Equal(after.DeletionTimestamp.Time), "marked %s, read back as %s", before.DeletionTimestamp.Time, after.DeletionTimestamp.Time)
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

// "default-2/a" comes before "default/b" as text, because "-" comes before
// "/". As namespaces, "default" comes first.
func TestListOrdersByNamespaceAndThenByName(t *testing.T) {
	servers := GameServersOf(open(t, filepath.Join(t.TempDir(), "state.db")))
	for _, key := range [][2]string{{"default-2", "a"}, {"default", "b"}, {"default", "a"}} {
		gs := gameServer(key[1])
		gs.Namespace = key[0]
		_, err := servers.Create(gs)
		require.NoError(t, err)
	}

	var listed []string
	for _, gs := range servers.List("") {
		listed = append(listed, gs.Namespace+"/"+gs.Name)
	}
	assert.Equal(t, []string{"default/a", "default/b", "default-2/a"}, listed)
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

// While Run saves in the background a write does not wait for the file,
// and Sync does. What Sync has waited for outlasts a crash.
func TestSyncWaitsForTheFileWhileRunSavesBehind(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s := open(t, path)
	ctx, stop := context.WithCancel(context.Background())
	var running sync.WaitGroup
	running.Go(func() { s.Run(ctx) })
	defer running.Wait()
	defer stop()

	servers := GameServersOf(s)
	for i := range 200 {
		_, err := servers.Create(gameServer(fmt.Sprint("gs-", i)))
		require.NoError(t, err)
	}
	require.NoError(t, servers.Sync(context.Background()))
	require.NoError(t, s.Abandon())

	assert.Len(t, GameServersOf(open(t, path)).List(""), 200)
}

// Run does its saving when it is told to stop, too.
func TestWhatRunHasNotSavedItSavesWhenItStops(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s := open(t, path)
	ctx, stop := context.WithCancel(context.Background())
	var running sync.WaitGroup
	running.Go(func() { s.Run(ctx) })

	_, err := GameServersOf(s).Create(gameServer("a"))
	require.NoError(t, err)
	stop()
	running.Wait()
	require.NoError(t, s.Abandon())

	assert.Len(t, GameServersOf(open(t, path)).List(""), 1)
}

// With nobody saving behind, the file comes first: a change it refuses was
// never made, as far as anyone can tell.
func TestAChangeTheFileRefusesIsTakenBack(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "state.db"))
	servers := GameServersOf(s)
	kept, err := servers.Create(gameServer("kept"))
	require.NoError(t, err)
	events, cancel := servers.Watch()
	defer cancel()
	require.NoError(t, s.file.db.Close())

	_, createErr := servers.Create(gameServer("new"))
	_, updateErr := servers.Update(kept)
	deleteErr := servers.Delete("default", "kept")

	assert.Error(t, createErr)
	assert.Error(t, updateErr)
	assert.Error(t, deleteErr)
	assert.Equal(t, []*agonesv1.GameServer{kept}, servers.List(""))
	assert.Equal(t, []*agonesv1.GameServer{kept}, OnNode(servers, ""), "and is in no group")
	assert.Empty(t, events, "nobody is told of a change that was taken back")
}

func TestOnNodeFindsTheGameServersOfANodeAndFollowsThemAsTheyMove(t *testing.T) {
	servers := GameServersOf(open(t, filepath.Join(t.TempDir(), "state.db")))
	names := func(node string) []string {
		var out []string
		for _, gs := range OnNode(servers, node) {
			out = append(out, gs.Name)
		}
		slices.Sort(out)
		return out
	}
	a, err := servers.Create(gameServer("a"))
	require.NoError(t, err)
	_, err = servers.Create(gameServer("b"))
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "b"}, names(""), "neither has a node yet")

	a.Status.NodeName = "node-1"
	_, err = servers.Update(a)
	require.NoError(t, err)
	assert.Equal(t, []string{"a"}, names("node-1"))
	assert.Equal(t, []string{"b"}, names(""))

	require.NoError(t, servers.Delete("default", "a"))
	assert.Empty(t, names("node-1"))
}

// Five random characters do not go far among ten thousand names. A name
// that is taken is drawn again, and only one that keeps coming up is given
// up on.
func TestAGeneratedNameThatIsTakenIsDrawnAgain(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "state.db"))
	draws := []string{"aaaaa", "aaaaa", "aaaaa", "bbbbb"}
	s.Suffix = func() string {
		next := draws[0]
		draws = append(draws[1:], next) // the last one again and again
		return next
	}
	servers := GameServersOf(s)
	generated := func() (*agonesv1.GameServer, error) {
		gs := gameServer("")
		gs.GenerateName = "fleet-"
		return servers.Create(gs)
	}

	first, err := generated()
	require.NoError(t, err)
	second, err := generated()
	require.NoError(t, err)
	_, err = generated()

	assert.Equal(t, []string{"fleet-aaaaa", "fleet-bbbbb"}, []string{first.Name, second.Name})
	assert.ErrorIs(t, err, ErrExists, "every draw gave a name that is taken")
}

// One save may hold more changes than a watcher has room for, and they
// reach it all at once. A watcher that is reading is waited for.
func TestAWatcherThatKeepsReadingGetsALargeSaveWhole(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "state.db"))
	s.behind.Store(true) // written behind, and saved by hand below
	servers := GameServersOf(s)
	events, cancel := servers.Watch()
	defer cancel()
	const changes = 3 * watchBuffer
	heard := make(chan int)
	go func() {
		n := 0
		for range events {
			if n++; n == changes {
				break
			}
		}
		heard <- n
	}()

	gs, err := servers.Create(gameServer("a"))
	require.NoError(t, err)
	for range changes - 1 {
		gs, err = servers.Update(gs)
		require.NoError(t, err)
	}
	require.NoError(t, s.saveBehind())

	assert.Equal(t, changes, <-heard)
}

// A watcher that does not read is waited for a moment, and then closed.
// Whoever writes is not kept waiting meanwhile.
func TestAWatcherThatIsStuckIsClosedAfterAMoment(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "state.db"))
	s.behind.Store(true)
	servers := GameServersOf(s)
	stuck, cancel := servers.Watch()
	defer cancel()

	gs, err := servers.Create(gameServer("a"))
	require.NoError(t, err)
	for range watchBuffer { // one change more than it has room for
		gs, err = servers.Update(gs)
		require.NoError(t, err)
	}
	saved := make(chan error)
	go func() { saved <- s.saveBehind() }()
	_, err = servers.Update(gs) // while the save waits for the watcher
	require.NoError(t, err)
	require.NoError(t, <-saved)

	for range watchBuffer {
		_, open := <-stuck
		require.True(t, open, "what it had room for is still there to read")
	}
	_, open := <-stuck
	assert.False(t, open, "and then its channel ends")
}

// Run tells watchers without the store's lock, so a watcher may leave
// between the save and the telling. It is then simply not told.
func TestAWatcherThatLeftBeforeItWasToldIsLeftAlone(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "state.db"))
	s.behind.Store(true)
	servers := GameServersOf(s)
	_, err := servers.Create(gameServer("a"))
	require.NoError(t, err)
	saved := s.unsaved

	for range 100 { // telling used to send on the closed channel half the time
		_, cancel := s.Watch(GameServers)
		s.mu.Lock()
		listening := s.listening()
		s.mu.Unlock()
		for _, w := range listening {
			w.after = 0 // as if it had been watching before the change
		}
		cancel()
		for _, w := range listening {
			assert.True(t, tellAll(w, saved, patience))
		}
	}
}

// running starts Run on s, in a bubble, and returns what stops it and waits
// for it. It returns once Run has taken over the saving: a write made
// before that would save itself.
func running(s *Store) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	var run sync.WaitGroup
	run.Go(func() { s.Run(ctx) })
	synctest.Wait()
	return func() { cancel(); run.Wait() }
}

// stalled is a disk that takes nothing until it is released.
func stalled() (disk func() error, release func()) {
	gate := make(chan struct{})
	return func() error { <-gate; return nil }, sync.OnceFunc(func() { close(gate) })
}

// The tests below run in a bubble of testing/synctest: time is the
// bubble's own and moves only when everyone in it waits, so "after three
// seconds" is exact, and takes none.

func TestSyncGivesUpWhenItsCallerDoesAndTheChangeIsSavedAllTheSame(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "state.db")
		s := open(t, path)
		disk, release := stalled()
		s.Disk = disk
		stop := running(s)
		_, err := GameServersOf(s).Create(gameServer("a"))
		require.NoError(t, err)

		impatient, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		asked := time.Now()
		err = s.Sync(impatient)

		assert.ErrorIs(t, err, context.DeadlineExceeded)
		assert.Equal(t, 100*time.Millisecond, time.Since(asked), "it waited for as long as its caller, and no longer")

		release()
		require.NoError(t, s.Sync(context.Background()))
		stop()
		require.NoError(t, s.Abandon())
		assert.Len(t, GameServersOf(open(t, path)).List(""), 1)
	})
}

func TestLagTellsSinceWhenTheOldestUnsavedChangeHasWaited(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := open(t, filepath.Join(t.TempDir(), "state.db"))
		disk, release := stalled()
		s.Disk = disk
		stop := running(s)
		defer stop()
		servers := GameServersOf(s)
		assert.Zero(t, s.Lag(), "the file has everything")

		_, err := servers.Create(gameServer("first"))
		require.NoError(t, err)
		time.Sleep(2 * time.Second)
		_, err = servers.Create(gameServer("second"))
		require.NoError(t, err)
		time.Sleep(time.Second)

		assert.Equal(t, 3*time.Second, s.Lag(), "the first has waited longest")

		release()
		require.NoError(t, s.Sync(context.Background()))
		assert.Zero(t, s.Lag())
	})
}

func TestADiskThatFailsIsTriedAgainAndNothingIsPromisedMeanwhile(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "state.db")
		s := open(t, path)
		failures := 3
		s.Disk = func() error {
			if failures > 0 {
				failures--
				return errors.New("input/output error")
			}
			return nil
		}
		stop := running(s)
		_, err := GameServersOf(s).Create(gameServer("a"))
		require.NoError(t, err)

		asked := time.Now()
		require.NoError(t, s.Sync(context.Background()))

		assert.Equal(t, 3*retryAfter, time.Since(asked), "Sync returned with the first save that worked")
		stop()
		require.NoError(t, s.Abandon())
		assert.Len(t, GameServersOf(open(t, path)).List(""), 1)
	})
}
