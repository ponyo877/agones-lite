package store

import (
	"cmp"
	"database/sql"
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
	mu       sync.Mutex
	db       *sql.DB
	revision int64
	// objects holds everything, by kind. A stored object is never changed
	// in place: an update swaps in a new one. That is what makes View safe.
	objects   map[Kind]map[Key]Object
	factories map[Kind]func() Object
	watchers  map[*watcher]struct{}
}

// Open loads the SQLite file at path, creating it when missing.
// kinds tells the store how to make an empty object of each kind.
func Open(path string, kinds map[Kind]func() Object) (*Store, error) {
	db, err := openDB(path)
	if err != nil {
		return nil, err
	}
	s := &Store{
		db:        db,
		objects:   map[Kind]map[Key]Object{},
		factories: kinds,
		watchers:  map[*watcher]struct{}{},
	}
	if err := s.load(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the SQLite file.
func (s *Store) Close() error {
	return s.db.Close()
}

// Get returns a private copy of the object, so callers may change it freely.
func (s *Store) Get(key Key) (Object, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	obj, ok := s.objects[key.Kind][key]
	if !ok {
		return nil, false
	}
	return clone(obj), true
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
	if err := prepare(obj); err != nil {
		return nil, err
	}
	key := KeyOf(kind, obj)
	if _, ok := s.objects[kind][key]; ok {
		return nil, fmt.Errorf("%s: %w", key, ErrExists)
	}
	return s.commit(Added, key, obj)
}

// Update replaces an object. It fails with ErrConflict unless obj carries the
// resourceVersion of the stored object.
func (s *Store) Update(kind Kind, obj Object) (Object, error) {
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
	return s.commit(Modified, key, clone(obj))
}

// Delete removes an object for good.
func (s *Store) Delete(key Key) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	obj, ok := s.objects[key.Kind][key]
	if !ok {
		return fmt.Errorf("%s: %w", key, ErrNotFound)
	}
	next := s.revision + 1
	if err := s.remove(key, next); err != nil {
		return err
	}
	s.revision = next
	delete(s.objects[key.Kind], key)
	s.notify(Event{Type: Deleted, Key: key, Object: obj})
	return nil
}

// commit writes obj under the next revision: disk first, then memory, then watchers.
func (s *Store) commit(t EventType, key Key, obj Object) (Object, error) {
	next := s.revision + 1
	obj.SetResourceVersion(strconv.FormatInt(next, 10))
	if err := s.persist(key, next, obj); err != nil {
		return nil, err
	}
	s.revision = next
	s.put(key, obj)
	s.notify(Event{Type: t, Key: key, Object: obj})
	return clone(obj), nil
}

func (s *Store) put(key Key, obj Object) {
	if s.objects[key.Kind] == nil {
		s.objects[key.Kind] = map[Key]Object{}
	}
	s.objects[key.Kind][key] = obj
}

// prepare fills in what the API server would on creation.
func prepare(obj Object) error {
	if obj.GetName() == "" {
		if obj.GetGenerateName() == "" {
			return errors.New("metadata.name or metadata.generateName is required")
		}
		obj.SetName(generatedName(obj.GetGenerateName()))
	}
	obj.SetUID(uuid.NewUUID())
	obj.SetCreationTimestamp(toTheSecond(time.Now()))
	obj.SetGeneration(1)
	return nil
}

// The shape of a generated name, as in Kubernetes
// (k8s.io/apiserver/pkg/storage/names): the base, cut short if need be,
// and five random characters, which together fit a DNS label.
const (
	maxNameLength = 63
	randomLength  = 5
)

// generatedName makes a name from a generateName. Without the cut, the
// GameServers of a Fleet with a long name would get names too long for a
// label, which is where their names also go.
func generatedName(base string) string {
	if most := maxNameLength - randomLength; len(base) > most {
		base = base[:most]
	}
	return base + rand.String(randomLength)
}

// toTheSecond is t the way the file keeps it. JSON holds a time to the
// second, so a finer one in memory would change when the file is read
// again: two objects made within one second would be in order before a
// restart and tied after it.
func toTheSecond(t time.Time) metav1.Time {
	return metav1.NewTime(t.Truncate(time.Second))
}

func clone(obj Object) Object {
	return obj.DeepCopyObject().(Object)
}
