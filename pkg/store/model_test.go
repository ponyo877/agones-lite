package store

import (
	"cmp"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"pgregory.net/rapid"
)

// This test lets rapid do random things to a store and, next to it, to a
// model of the store: a plain map. After every step the two must agree. The
// model is too simple to be wrong in the ways a store can be, so where they
// differ, the store is wrong. A failure is printed as the shortest sequence
// of steps that leads to it.
//
// What must hold, and where it is checked:
//
//   - The revision only grows: by one for every change, and not at all for
//     an operation that is refused (Check).
//   - An Update with a stale resourceVersion is a conflict and changes
//     nothing (UpdateWithAStaleVersion, then Check).
//   - Get, List and View show what the model holds (Check).
//   - After the file is opened again, memory holds what it held before
//     (Reopen, then Check).
//   - Where the file is written behind (Store.Run), a crash takes back the
//     changes made since the last save, all of them and nothing else, and
//     a watcher is told of a change when it is saved (Save, Crash, Check).
//   - What View or a Watch event handed out is never changed by a later
//     write (Check).
//   - The GameServers the store finds for a node are the ones a look at
//     every GameServer finds (Check).
//   - A watcher that keeps up is told every change, once and in order (Check).

// Two kinds, so that a change to one can be mistaken for a change to the
// other. Both hold GameServers: the store does not care what a kind means.
const (
	red  Kind = "red"
	blue Kind = "blue"
)

var modelKinds = map[Kind]func() Object{
	red:  func() Object { return &agonesv1.GameServer{} },
	blue: func() Object { return &agonesv1.GameServer{} },
}

// A few namespaces and names, so that the same key comes up again and again.
// "a-b" sorts before "a/" as text, and after "a" as a namespace.
var (
	modelNamespaces = []string{"a", "a-b", "b"}
	modelNames      = []string{"x", "y", "z"}
	modelNodes      = []string{"", "n1", "n2"} // an object may have none
)

// storeModel is a store, and beside it what the store should hold.
type storeModel struct {
	path  string
	store *Store

	objects  map[Key]*agonesv1.GameServer // what was stored, by key
	revision int64                        // how many changes were committed
	outdated map[Key][]string             // resourceVersions each key no longer has
	// behind says that the file is written behind, as while Store.Run runs:
	// here at the Save steps, so that a run can be repeated. inFile is the
	// model as it was at the last save, which is all a crash leaves.
	behind         bool
	inFile         map[Key]*agonesv1.GameServer
	inFileRevision int64
	// lent holds every object the store shared with a reader, through View
	// or a Watch event, with a copy of how it looked when it was handed out.
	lent     map[Object]Object
	watchers []*follower
}

// follower is a watcher and the changes it has yet to be told: those made
// since it began and not saved yet, and those it is owed by now.
type follower struct {
	kind   Kind
	events <-chan Event
	cancel func()
	made   []Event
	owed   []Event
}

func TestTheStoreBehavesLikeAMap(t *testing.T) {
	dir := scratch(t)
	rapid.Check(t, func(t *rapid.T) {
		file, err := os.CreateTemp(dir, "*.db")
		require.NoError(t, err)
		require.NoError(t, file.Close())
		m := &storeModel{path: file.Name(), outdated: map[Key][]string{}, lent: map[Object]Object{}}
		m.objects, m.inFile = map[Key]*agonesv1.GameServer{}, map[Key]*agonesv1.GameServer{}
		m.behind = rapid.Bool().Draw(t, "the file is written behind")
		m.open(t)
		defer func() { _ = m.store.Close() }()

		t.Repeat(rapid.StateMachineActions(m))
	})
}

// scratch is a directory for the files of this test: in memory where the
// system has a place for that, as Linux does. The test commits thousands
// of times, and waiting for a disk each time would say nothing about what
// it checks, which is not whether the disk keeps what it was given.
func scratch(t *testing.T) string {
	dir, err := os.MkdirTemp("/dev/shm", "agones-lite-store-")
	if err != nil {
		return t.TempDir()
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func (m *storeModel) open(t *rapid.T) {
	s, err := Open(m.path, modelKinds)
	require.NoError(t, err)
	s.behind.Store(m.behind)
	m.store = s
}

func (m *storeModel) typed(kind Kind) Typed[*agonesv1.GameServer] {
	return Typed[*agonesv1.GameServer]{store: m.store, kind: kind}
}

// The steps. rapid calls every exported method but Check as one.

func (m *storeModel) Create(t *rapid.T) {
	key, obj := anyKey(t), anObject(t)
	obj.Namespace, obj.Name = key.Namespace, key.Name

	created, err := m.store.Create(key.Kind, obj)

	if _, exists := m.objects[key]; exists {
		require.ErrorIs(t, err, ErrExists)
		return
	}
	require.NoError(t, err)
	m.commit(Added, key, created)
	requireSame(t, m.objects[key], created, "what Create returned")
	assert.NotEmpty(t, created.GetUID())
	assert.Equal(t, int64(1), created.GetGeneration())
}

// A generated name is the base and five more characters. Kubernetes cuts
// the base so that the two together are a name of at most 63 characters,
// and what is made from a Fleet with a long name counts on that.
func (m *storeModel) CreateWithAGeneratedName(t *rapid.T) {
	key, obj := anyKey(t), anObject(t)
	base := strings.Repeat("f", rapid.IntRange(1, 70).Draw(t, "length of the base")) + "-"
	obj.Namespace, obj.GenerateName = key.Namespace, base

	created, err := m.store.Create(key.Kind, obj)

	require.NoError(t, err)
	kept := base[:min(len(base), 58)]
	require.Regexp(t, "^"+kept+"[a-z0-9]{5}$", created.GetName())
	key.Name = created.GetName()
	m.commit(Added, key, created)
	requireSame(t, m.objects[key], created, "what Create returned")
}

func (m *storeModel) Update(t *rapid.T) {
	key := m.aStoredKey(t)
	changed := anObject(t)
	changed.ObjectMeta = *m.objects[key].ObjectMeta.DeepCopy()
	changed.Labels = anObject(t).Labels

	updated, err := m.store.Update(key.Kind, changed)

	require.NoError(t, err)
	m.commit(Modified, key, changed)
	requireSame(t, m.objects[key], updated, "what Update returned")
}

func (m *storeModel) UpdateWithAStaleVersion(t *rapid.T) {
	key := m.aStoredKey(t)
	current := m.objects[key].ResourceVersion
	stale := m.objects[key].DeepCopy()
	stale.Labels = anObject(t).Labels
	versions := append([]string{"", "0", strconv.FormatInt(m.revision+1, 10)}, m.outdated[key]...)
	stale.ResourceVersion = rapid.SampledFrom(versions).Filter(func(v string) bool { return v != current }).Draw(t, "version")

	_, err := m.store.Update(key.Kind, stale)

	require.ErrorIs(t, err, ErrConflict)
}

func (m *storeModel) UpdateWhatIsNotThere(t *rapid.T) {
	key := m.aMissingKey(t)
	obj := anObject(t)
	obj.Namespace, obj.Name = key.Namespace, key.Name

	_, err := m.store.Update(key.Kind, obj)

	require.ErrorIs(t, err, ErrNotFound)
}

func (m *storeModel) Change(t *rapid.T) {
	key := anyKey(t)
	labels := anObject(t).Labels
	keep := rapid.Bool().Draw(t, "keep")

	err := m.typed(key.Kind).Change(key.Namespace, key.Name, func(gs *agonesv1.GameServer) bool {
		gs.Labels = labels
		return keep
	})

	require.NoError(t, err, "there is nothing to change about an object that is not there")
	if was, exists := m.objects[key]; exists && keep {
		want := was.DeepCopy()
		want.Labels = labels
		m.commit(Modified, key, want)
	}
}

// The mark is a time, and a time is kept to the second: that is all JSON
// holds of it, and so all the file would give back.
func (m *storeModel) MarkDeleted(t *rapid.T) {
	key := anyKey(t)
	now := time.Unix(rapid.Int64Range(1, 4_000_000_000).Draw(t, "seconds"), rapid.Int64Range(0, 999_999_999).Draw(t, "nanoseconds"))

	err := m.typed(key.Kind).MarkDeleted(key.Namespace, key.Name, now)

	require.NoError(t, err)
	if was, exists := m.objects[key]; exists && was.DeletionTimestamp == nil {
		want := was.DeepCopy()
		want.DeletionTimestamp = &metav1.Time{Time: now.Truncate(time.Second)}
		m.commit(Modified, key, want)
	}
}

func (m *storeModel) Delete(t *rapid.T) {
	key := anyKey(t)

	err := m.store.Delete(key)

	last, exists := m.objects[key]
	if !exists {
		require.ErrorIs(t, err, ErrNotFound)
		return
	}
	require.NoError(t, err)
	m.revision++
	delete(m.objects, key)
	m.owe(Event{Type: Deleted, Key: key, Object: last})
}

// Save is one round of what Store.Run does in the background.
func (m *storeModel) Save(t *rapid.T) {
	if !m.behind {
		t.Skip("every write saves itself")
	}
	require.NoError(t, m.store.saveBehind())
	m.fileHasIt()
}

// A crash takes with it what the file did not have yet, and those who
// watched. What the file had is there when the store is opened again.
func (m *storeModel) Crash(t *rapid.T) {
	require.NoError(t, m.store.Abandon())
	m.watchers = nil
	m.objects, m.revision = maps.Clone(m.inFile), m.inFileRevision
	m.open(t)
}

func (m *storeModel) Watch(t *rapid.T) {
	kind := aKind(t)
	events, cancel := m.store.Watch(kind)
	m.watchers = append(m.watchers, &follower{kind: kind, events: events, cancel: cancel})
}

func (m *storeModel) ListAndWatch(t *rapid.T) {
	kind, namespace := aKind(t), aNamespaceOrAll(t)

	listed, events, cancel := m.store.ListAndWatch(kind, namespace)

	m.sameList(t, m.stored(kind, namespace), listed, "ListAndWatch")
	m.watchers = append(m.watchers, &follower{kind: kind, events: events, cancel: cancel})
}

func (m *storeModel) StopWatching(t *rapid.T) {
	if len(m.watchers) == 0 {
		t.Skip("nobody is watching")
	}
	i := rapid.IntRange(0, len(m.watchers)-1).Draw(t, "watcher")
	w := m.watchers[i]
	m.watchers = slices.Delete(m.watchers, i, i+1)

	w.cancel()

	_, open := <-w.events
	assert.False(t, open, "cancel closes the channel")
	w.cancel() // a second time is harmless
}

// Get and List hand out private copies. Whatever is done to them, the
// store must not notice: Check will.
func (m *storeModel) ScribbleOnCopies(t *rapid.T) {
	key := m.aStoredKey(t)
	got, _ := m.store.Get(key)
	got.SetLabels(map[string]string{"scribbled": "on a copy from Get"})
	for _, obj := range m.store.List(key.Kind, "") {
		obj.SetLabels(map[string]string{"scribbled": "on a copy from List"})
	}
}

// Closing and opening the file is what a restart of the control plane does.
// Those who watched the closed store are gone with it.
func (m *storeModel) Reopen(t *rapid.T) {
	require.NoError(t, m.store.Close())
	m.fileHasIt() // closing saves
	m.watchers = nil
	m.open(t)
}

// Check holds the store against the model. rapid calls it after every step.
func (m *storeModel) Check(t *rapid.T) {
	m.store.mu.Lock()
	revision, saved := m.store.revision, m.store.saved.Load()
	m.store.mu.Unlock()
	require.Equal(t, m.revision, revision, "the revision counts the changes, and nothing else")
	require.Equal(t, m.inFileRevision, saved, "the revision the file holds")

	for _, kind := range []Kind{red, blue} {
		for _, namespace := range append([]string{""}, modelNamespaces...) {
			m.listsAgree(t, kind, namespace)
		}
	}
	for _, key := range everyKey() {
		m.getAgrees(t, key)
	}
	for _, kind := range []Kind{red, blue} {
		m.groupsAgree(t, kind)
	}
	for object, was := range m.lent {
		require.Equal(t, was, object, "an object handed out by View or Watch was changed afterwards")
	}
	for _, w := range m.watchers {
		m.isUpToDate(t, w)
	}
}

func (m *storeModel) listsAgree(t *rapid.T, kind Kind, namespace string) {
	want := m.stored(kind, namespace)
	m.sameList(t, want, m.store.List(kind, namespace), "List")

	viewed := m.store.View(kind, namespace)
	m.lend(viewed...)
	slices.SortFunc(viewed, byNamespaceAndName) // View promises no order
	m.sameList(t, want, viewed, "View")
}

// groupsAgree asks the store for the GameServers of each node, and for
// those without one, and holds each answer against the model's objects
// sorted the slow way. The store keeps its groups current with every
// write, and with every write that is taken back.
func (m *storeModel) groupsAgree(t *rapid.T, kind Kind) {
	for _, node := range modelNodes {
		var want []Object
		for _, obj := range m.stored(kind, "") {
			if obj.(*agonesv1.GameServer).Status.NodeName == node {
				want = append(want, obj)
			}
		}
		var got []Object
		for _, gs := range OnNode(m.typed(kind), node) {
			got = append(got, gs)
		}
		m.lend(got...)
		slices.SortFunc(got, byNamespaceAndName)
		m.sameList(t, want, got, "the group of node "+strconv.Quote(node))
		byPlace := m.memberByMember(kind, node)
		slices.SortFunc(byPlace, byNamespaceAndName)
		m.sameList(t, want, byPlace, "the group of node "+strconv.Quote(node)+", taken member by member")
	}
}

// memberByMember collects a group through Member, place after place, until
// there is no such place.
func (m *storeModel) memberByMember(kind Kind, node string) []Object {
	var members []Object
	for place := 0; ; place++ {
		gs, there := Member(m.typed(kind), byNode, node, place)
		if !there {
			return members
		}
		members = append(members, gs)
	}
}

func (m *storeModel) getAgrees(t *rapid.T, key Key) {
	got, ok := m.store.Get(key)
	want, stored := m.objects[key]
	require.Equal(t, stored, ok, "Get %s", key)
	if stored {
		requireSame(t, want, got, "Get "+key.String())
	}
}

// isUpToDate reads what a watcher was told and holds it against what it is
// owed. A change is in the channel by the time it is saved, so nothing
// here has to wait.
func (m *storeModel) isUpToDate(t *rapid.T, w *follower) {
	for _, want := range w.owed {
		got, told := heard(w)
		require.True(t, told, "%s %s did not reach a watcher", want.Type, want.Key)
		require.Equal(t, []any{want.Type, want.Key}, []any{got.Type, got.Key}, "the next change a watcher is told")
		requireSame(t, want.Object, got.Object, "the object of "+string(got.Type)+" "+got.Key.String())
		m.lend(got.Object)
	}
	w.owed = nil
	got, told := heard(w)
	require.False(t, told, "a watcher was told more than happened: %v", got)
}

// heard takes the next event from a watcher, if one is waiting. A closed
// channel has none: a watcher that keeps up is never closed.
func heard(w *follower) (Event, bool) {
	select {
	case event, open := <-w.events:
		return event, open
	default:
		return Event{}, false
	}
}

// commit records in the model a change the store accepted: stored is what
// the store must now hold under key, but for its resourceVersion, which is
// the number of the change.
func (m *storeModel) commit(change EventType, key Key, stored Object) {
	m.revision++
	gs := stored.DeepCopyObject().(*agonesv1.GameServer)
	gs.ResourceVersion = strconv.FormatInt(m.revision, 10)
	if was, existed := m.objects[key]; existed {
		m.outdated[key] = append(m.outdated[key], was.ResourceVersion)
	}
	m.objects[key] = gs
	m.owe(Event{Type: change, Key: key, Object: gs})
}

// owe notes a change for everyone who watches its kind. They are told
// when it is saved, which is at once unless the file is written behind.
func (m *storeModel) owe(change Event) {
	for _, w := range m.watchers {
		if w.kind == change.Key.Kind {
			w.made = append(w.made, change)
		}
	}
	if !m.behind {
		m.fileHasIt()
	}
}

// fileHasIt records that everything so far was saved.
func (m *storeModel) fileHasIt() {
	m.inFile, m.inFileRevision = maps.Clone(m.objects), m.revision
	for _, w := range m.watchers {
		w.owed, w.made = append(w.owed, w.made...), nil
	}
}

func (m *storeModel) lend(objects ...Object) {
	for _, obj := range objects {
		if _, before := m.lent[obj]; !before {
			m.lent[obj] = obj.DeepCopyObject().(Object)
		}
	}
}

// stored is what List should return: the model's objects of one kind, in
// namespace or in all, ordered by namespace and then by name.
func (m *storeModel) stored(kind Kind, namespace string) []Object {
	var out []Object
	for key, obj := range m.objects {
		if key.Kind == kind && (namespace == "" || key.Namespace == namespace) {
			out = append(out, obj)
		}
	}
	slices.SortFunc(out, byNamespaceAndName)
	return out
}

func byNamespaceAndName(a, b Object) int {
	return cmp.Or(strings.Compare(a.GetNamespace(), b.GetNamespace()), strings.Compare(a.GetName(), b.GetName()))
}

func (m *storeModel) sameList(t *rapid.T, want, got []Object, what string) {
	require.Equal(t, describe(want), describe(got), "%s returns the stored objects, in order", what)
	for i := range want {
		requireSame(t, want[i], got[i], what)
	}
}

func describe(objects []Object) []string {
	out := []string{}
	for _, obj := range objects {
		out = append(out, obj.GetNamespace()+"/"+obj.GetName()+"@"+obj.GetResourceVersion())
	}
	return out
}

// requireSame compares the way Kubernetes does: a time is the instant it
// names, whatever its time zone, and an empty map is no map.
func requireSame(t *rapid.T, want, got Object, what string) {
	if apiequality.Semantic.DeepEqual(want, got) {
		return
	}
	a, b := want.(*agonesv1.GameServer), got.(*agonesv1.GameServer)
	parts := []struct {
		name      string
		want, got any
	}{
		{"metadata", a.ObjectMeta, b.ObjectMeta},
		{"spec", a.Spec, b.Spec},
		{"status", a.Status, b.Status},
	}
	for _, part := range parts {
		if !apiequality.Semantic.DeepEqual(part.want, part.got) {
			require.Fail(t, what+" differs from what was stored", "%s\nwant %+v\ngot  %+v", part.name, part.want, part.got)
		}
	}
}

// The values rapid draws.

func aKind(t *rapid.T) Kind {
	return rapid.SampledFrom([]Kind{red, blue}).Draw(t, "kind")
}

func aNamespaceOrAll(t *rapid.T) string {
	return rapid.SampledFrom(append([]string{""}, modelNamespaces...)).Draw(t, "namespace")
}

// anyKey is a key that may or may not be stored.
func anyKey(t *rapid.T) Key {
	return rapid.SampledFrom(everyKey()).Draw(t, "key")
}

func everyKey() []Key {
	var keys []Key
	for _, kind := range []Kind{red, blue} {
		for _, namespace := range modelNamespaces {
			for _, name := range modelNames {
				keys = append(keys, Key{Kind: kind, Namespace: namespace, Name: name})
			}
		}
	}
	return keys
}

func (m *storeModel) aStoredKey(t *rapid.T) Key {
	if len(m.objects) == 0 {
		t.Skip("nothing is stored")
	}
	keys := slices.SortedFunc(maps.Keys(m.objects), func(a, b Key) int { return strings.Compare(a.String(), b.String()) })
	return rapid.SampledFrom(keys).Draw(t, "stored key")
}

func (m *storeModel) aMissingKey(t *rapid.T) Key {
	missing := slices.DeleteFunc(everyKey(), func(key Key) bool { return m.objects[key] != nil })
	if len(missing) == 0 {
		t.Skip("every key is stored")
	}
	return rapid.SampledFrom(missing).Draw(t, "missing key")
}

// anObject is a GameServer with something in it that tells one version
// from the next. Its own times are whole seconds, as its callers' should be.
func anObject(t *rapid.T) *agonesv1.GameServer {
	gs := &agonesv1.GameServer{}
	gs.Labels = map[string]string{"colour": rapid.SampledFrom([]string{"green", "amber", "violet"}).Draw(t, "label")}
	gs.Status.State = rapid.SampledFrom([]agonesv1.GameServerState{"", agonesv1.GameServerStateReady, agonesv1.GameServerStateAllocated}).Draw(t, "state")
	gs.Status.NodeName = rapid.SampledFrom(modelNodes).Draw(t, "node")
	if rapid.Bool().Draw(t, "reserved") {
		gs.Status.ReservedUntil = &metav1.Time{Time: time.Unix(rapid.Int64Range(1, 4_000_000_000).Draw(t, "until"), 0)}
	}
	return gs
}
