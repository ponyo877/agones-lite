package store

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/apimachinery/pkg/util/uuid"
)

// Errors returned by the write operations.
var (
	ErrNotFound = errors.New("not found")
	ErrExists   = errors.New("already exists")
	ErrConflict = errors.New("changed by someone else, read it again")
)

// Kind names a type of resource, in the plural form the API uses.
type Kind string

// Key identifies one resource. Namespace is empty for cluster-wide kinds.
type Key struct {
	Kind      Kind
	Namespace string
	Name      string
}

func (k Key) String() string {
	return fmt.Sprintf("%s/%s/%s", k.Kind, k.Namespace, k.Name)
}

// Object is anything the store can hold: the Agones types and our own.
type Object interface {
	metav1.Object
	runtime.Object
}

// KeyOf returns the key obj is stored under.
func KeyOf(kind Kind, obj metav1.Object) Key {
	return Key{Kind: kind, Namespace: obj.GetNamespace(), Name: obj.GetName()}
}

// Store is safe for concurrent use.
type Store struct {
	mu sync.Mutex
	// revision numbers the last change made, and objects holds everything
	// as of that change, by kind. A stored object is never changed in
	// place: an update swaps in a new one. That is what makes View safe.
	revision  int64
	objects   map[Kind]map[Key]Object
	groupings map[Kind]map[string]*grouping // the same objects, sorted into groups: see group.go
	factories map[Kind]func() Object
	watchers  map[*watcher]struct{}

	// The file, and how far it is behind memory: see save.go.
	file    file
	unsaved []change   // made, but not in the file yet; oldest first
	saved   int64      // the revision the file holds
	settled *sync.Cond // told whenever saved moves
	behind  bool       // Run is saving: a write does not wait for the file
	wake    chan struct{}

	// Clock tells the time, and Suffix makes the random end of a generated
	// name. They are the machine's, unless a test puts its own in their
	// place before the store is used, to have the same run twice.
	Clock  func() time.Time
	Suffix func() string
}

// Open loads the SQLite file at path, creating it when missing.
// kinds tells the store how to make an empty object of each kind.
func Open(path string, kinds map[Kind]func() Object) (*Store, error) {
	f, err := openFile(path)
	if err != nil {
		return nil, err
	}
	s := &Store{
		file:      f,
		objects:   map[Kind]map[Key]Object{},
		groupings: map[Kind]map[string]*grouping{},
		factories: kinds,
		watchers:  map[*watcher]struct{}{},
		wake:      make(chan struct{}, 1),
		Clock:     time.Now,
		Suffix:    func() string { return rand.String(randomLength) },
	}
	s.settled = sync.NewCond(&s.mu)
	if err := s.load(); err != nil {
		_ = f.db.Close()
		return nil, err
	}
	s.saved = s.revision
	return s, nil
}

// Close saves what is not saved yet and releases the SQLite file. Run must
// have returned by then.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return errors.Join(s.save(), s.file.db.Close())
}

// Abandon releases the SQLite file without saving, which is what a crash
// does to a store. It is for tests of what a crash leaves behind.
func (s *Store) Abandon() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.file.db.Close()
}

// Now tells the time by the store's clock. Whatever works on a store asks
// it for the time, so that one clock moves a whole control plane.
func (s *Store) Now() time.Time {
	return s.Clock()
}

// Get returns a private copy of the object, so callers may change it freely.
// Like every read, it shows the changes made so far, saved or not: see Run.
func (s *Store) Get(key Key) (Object, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	obj, ok := s.objects[key.Kind][key]
	if !ok {
		return nil, false
	}
	return clone(obj), true
}

// Peek returns the stored object itself, as View does for all of a kind:
// it is shared, and strictly for reading. It is for those who look at an
// object to decide whether anything is to be done, and mostly find that
// nothing is.
func (s *Store) Peek(key Key) (Object, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	obj, ok := s.objects[key.Kind][key]
	return obj, ok
}

// List returns copies of every object of kind, ordered by namespace and name.
// An empty namespace means all namespaces.
func (s *Store) List(kind Kind, namespace string) []Object {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.list(kind, namespace)
}

// list is List with the lock held.
func (s *Store) list(kind Kind, namespace string) []Object {
	var out []Object
	for key, obj := range s.objects[kind] {
		if namespace == "" || key.Namespace == namespace {
			out = append(out, clone(obj))
		}
	}
	// Compared part by part. Joined into one text, "a-b/x" would come
	// before "a/x", and namespace "a-b" before namespace "a".
	slices.SortFunc(out, func(a, b Object) int {
		return cmp.Or(strings.Compare(a.GetNamespace(), b.GetNamespace()), strings.Compare(a.GetName(), b.GetName()))
	})
	return out
}

// View returns every object of kind without copying, in no particular order.
// An empty namespace means all namespaces.
//
// What it returns is shared with the store and with every other caller, so
// it is strictly for reading. It exists for the places that look at
// everything on every change, such as allocation: copying a thousand game
// servers each time is what would make them slow. To change an object, Get
// a private copy of it or use Typed.Change.
func (s *Store) View(kind Kind, namespace string) []Object {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Object, 0, len(s.objects[kind]))
	for key, obj := range s.objects[kind] {
		if namespace == "" || key.Namespace == namespace {
			out = append(out, obj)
		}
	}
	return out
}

// Create stores a new object and fills in its name, UID and creation time.
func (s *Store) Create(kind Kind, obj Object) (Object, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	obj = clone(obj)
	if err := s.prepare(kind, obj); err != nil {
		return nil, err
	}
	key := KeyOf(kind, obj)
	if _, ok := s.objects[kind][key]; ok {
		return nil, fmt.Errorf("%s: %w", key, ErrExists)
	}
	return copyOf(s.commit(Added, key, obj))
}

// Update replaces an object. It fails with ErrConflict unless obj carries the
// resourceVersion of the stored object.
func (s *Store) Update(kind Kind, obj Object) (Object, error) {
	return copyOf(s.replace(kind, clone(obj)))
}

// replace is Update for a caller that gives obj away: it is stored as it
// is, and what it returns is the stored object, shared. Update puts a copy
// on either side of it.
func (s *Store) replace(kind Kind, obj Object) (Object, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := KeyOf(kind, obj)
	current, ok := s.objects[kind][key]
	if !ok {
		return nil, fmt.Errorf("%s: %w", key, ErrNotFound)
	}
	if obj.GetResourceVersion() != current.GetResourceVersion() {
		return nil, fmt.Errorf("%s: %w", key, ErrConflict)
	}
	return s.commit(Modified, key, obj)
}

// Delete removes an object for good.
func (s *Store) Delete(key Key) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	obj, ok := s.objects[key.Kind][key]
	if !ok {
		return fmt.Errorf("%s: %w", key, ErrNotFound)
	}
	return s.record(change{event: Event{Type: Deleted, Key: key, Object: obj}, revision: s.revision + 1, was: obj})
}

// commit makes obj what is stored under key, as the next revision, and
// returns the stored object: shared, so not for the caller's caller.
//
// Memory gets what the file gets, read back, and not obj itself. The two
// differ in small ways: JSON keeps a time to the second, and an empty list
// may come back as none. A store that answers one thing before a restart
// and another after it would change, for one, the order of two GameServers
// made within a second, and with it which of them a scale-down removes.
func (s *Store) commit(t EventType, key Key, obj Object) (Object, error) {
	next := s.revision + 1
	obj.SetResourceVersion(strconv.FormatInt(next, 10))
	stored, data, err := s.asStored(key, obj)
	if err != nil {
		return nil, err
	}
	c := change{event: Event{Type: t, Key: key, Object: stored}, revision: next, was: s.objects[key.Kind][key], data: data}
	if err := s.record(c); err != nil {
		return nil, err
	}
	return stored, nil
}

// hold makes obj what memory has under key, or drops the key when obj is nil.
func (s *Store) hold(key Key, obj Object) {
	for _, g := range s.groupings[key.Kind] {
		g.move(key, s.objects[key.Kind][key], obj)
	}
	if obj == nil {
		delete(s.objects[key.Kind], key)
		return
	}
	if s.objects[key.Kind] == nil {
		s.objects[key.Kind] = map[Key]Object{}
	}
	s.objects[key.Kind][key] = obj
}

// prepare fills in what the API server would on creation.
func (s *Store) prepare(kind Kind, obj Object) error {
	if obj.GetName() == "" {
		if obj.GetGenerateName() == "" {
			return errors.New("metadata.name or metadata.generateName is required")
		}
		obj.SetName(s.freeName(kind, obj))
	}
	obj.SetUID(uuid.NewUUID())
	obj.SetCreationTimestamp(metav1.NewTime(s.Now()))
	obj.SetGeneration(1)
	return nil
}

// The shape of a generated name, as in Kubernetes
// (k8s.io/apiserver/pkg/storage/names): the base, cut short if need be,
// and five random characters, which together fit a DNS label.
const (
	maxNameLength = 63
	randomLength  = 5
	// nameDraws is how often a generated name is drawn before Create gives
	// up, as in Kubernetes (maxNameGenerationCreateAttempts).
	nameDraws = 8
)

// freeName generates a name for obj that nothing of its kind has yet. Five
// random characters make one name out of fourteen million, and among the
// ten thousand GameServers of a large Fleet two of those meet now and then.
// A name that is taken is drawn again; after nameDraws the last one is
// returned as it is, and Create says that it exists.
func (s *Store) freeName(kind Kind, obj Object) string {
	var name string
	for range nameDraws {
		name = generatedName(obj.GetGenerateName(), s.Suffix())
		if _, taken := s.objects[kind][Key{Kind: kind, Namespace: obj.GetNamespace(), Name: name}]; !taken {
			break
		}
	}
	return name
}

// generatedName makes a name from a generateName. Without the cut, the
// GameServers of a Fleet with a long name would get names too long for a
// label, which is where their names also go.
func generatedName(base, suffix string) string {
	if most := maxNameLength - randomLength; len(base) > most {
		base = base[:most]
	}
	return base + suffix
}

func clone(obj Object) Object {
	return obj.DeepCopyObject().(Object)
}

// copyOf turns the stored object a write returns into a copy for the
// caller, and passes an error on.
func copyOf(stored Object, err error) (Object, error) {
	if err != nil {
		return nil, err
	}
	return clone(stored), nil
}
