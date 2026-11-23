package store

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"sync"

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
	mu        sync.Mutex
	db        *sql.DB
	revision  int64
	objects   map[Key]Object
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
		objects:   map[Key]Object{},
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
	obj, ok := s.objects[key]
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
	var out []Object
	for key, obj := range s.objects {
		if key.Kind == kind && (namespace == "" || key.Namespace == namespace) {
			out = append(out, clone(obj))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := KeyOf(kind, out[i]), KeyOf(kind, out[j])
		return a.Namespace+"/"+a.Name < b.Namespace+"/"+b.Name
	})
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
	if _, ok := s.objects[key]; ok {
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
	current, ok := s.objects[key]
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
	obj, ok := s.objects[key]
	if !ok {
		return fmt.Errorf("%s: %w", key, ErrNotFound)
	}
	next := s.revision + 1
	if err := s.remove(key, next); err != nil {
		return err
	}
	s.revision = next
	delete(s.objects, key)
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
	s.objects[key] = obj
	s.notify(Event{Type: t, Key: key, Object: obj})
	return clone(obj), nil
}

// prepare fills in what the API server would on creation.
func prepare(obj Object) error {
	if obj.GetName() == "" {
		if obj.GetGenerateName() == "" {
			return errors.New("metadata.name or metadata.generateName is required")
		}
		obj.SetName(obj.GetGenerateName() + rand.String(5))
	}
	obj.SetUID(uuid.NewUUID())
	obj.SetCreationTimestamp(metav1.Now())
	obj.SetGeneration(1)
	return nil
}

func clone(obj Object) Object {
	return obj.DeepCopyObject().(Object)
}
