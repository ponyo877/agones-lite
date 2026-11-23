package store

import (
	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"

	litev1 "github.com/ponyo877/agones-lite/pkg/apis/lite/v1"
)

// The kinds agones-lite stores.
const (
	GameServers Kind = "gameservers"
	Nodes       Kind = "nodes"
)

// Kinds maps every kind to a constructor for its Go type.
func Kinds() map[Kind]func() Object {
	return map[Kind]func() Object{
		GameServers: func() Object { return &agonesv1.GameServer{} },
		Nodes:       func() Object { return &litev1.Node{} },
	}
}

// Typed is a view of the Store limited to one kind, with the Go type filled
// in so callers need no type assertions.
type Typed[T Object] struct {
	store *Store
	kind  Kind
}

// GameServersOf returns the GameServer view of s.
func GameServersOf(s *Store) Typed[*agonesv1.GameServer] {
	return Typed[*agonesv1.GameServer]{store: s, kind: GameServers}
}

// NodesOf returns the Node view of s.
func NodesOf(s *Store) Typed[*litev1.Node] {
	return Typed[*litev1.Node]{store: s, kind: Nodes}
}

// Get returns a private copy of the named object.
func (t Typed[T]) Get(namespace, name string) (T, bool) {
	obj, ok := t.store.Get(Key{Kind: t.kind, Namespace: namespace, Name: name})
	if !ok {
		var zero T
		return zero, false
	}
	return obj.(T), true
}

// List returns copies of every object in namespace, or in all when it is empty.
func (t Typed[T]) List(namespace string) []T {
	objs := t.store.List(t.kind, namespace)
	out := make([]T, len(objs))
	for i, obj := range objs {
		out[i] = obj.(T)
	}
	return out
}

// Create stores a new object.
func (t Typed[T]) Create(obj T) (T, error) {
	return t.result(t.store.Create(t.kind, obj))
}

// Update replaces an object, subject to the resourceVersion check.
func (t Typed[T]) Update(obj T) (T, error) {
	return t.result(t.store.Update(t.kind, obj))
}

// Delete removes the named object.
func (t Typed[T]) Delete(namespace, name string) error {
	return t.store.Delete(Key{Kind: t.kind, Namespace: namespace, Name: name})
}

// Watch delivers every change to this kind until cancel is called.
func (t Typed[T]) Watch() (events <-chan Event, cancel func()) {
	return t.store.Watch(t.kind)
}

func (t Typed[T]) result(obj Object, err error) (T, error) {
	if err != nil {
		var zero T
		return zero, err
	}
	return obj.(T), nil
}
