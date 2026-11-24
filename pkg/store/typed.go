package store

import (
	"errors"
	"time"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	autoscalingv1 "agones.dev/agones/pkg/apis/autoscaling/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	litev1 "github.com/ponyo877/agones-lite/pkg/apis/lite/v1"
)

// The kinds agones-lite stores.
const (
	GameServers      Kind = "gameservers"
	GameServerSets   Kind = "gameserversets"
	Fleets           Kind = "fleets"
	FleetAutoscalers Kind = "fleetautoscalers"
	Nodes            Kind = "nodes"
)

// Kinds maps every kind to a constructor for its Go type.
func Kinds() map[Kind]func() Object {
	return map[Kind]func() Object{
		GameServers:      func() Object { return &agonesv1.GameServer{} },
		GameServerSets:   func() Object { return &agonesv1.GameServerSet{} },
		Fleets:           func() Object { return &agonesv1.Fleet{} },
		FleetAutoscalers: func() Object { return &autoscalingv1.FleetAutoscaler{} },
		Nodes:            func() Object { return &litev1.Node{} },
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

// GameServerSetsOf returns the GameServerSet view of s.
func GameServerSetsOf(s *Store) Typed[*agonesv1.GameServerSet] {
	return Typed[*agonesv1.GameServerSet]{store: s, kind: GameServerSets}
}

// FleetsOf returns the Fleet view of s.
func FleetsOf(s *Store) Typed[*agonesv1.Fleet] {
	return Typed[*agonesv1.Fleet]{store: s, kind: Fleets}
}

// FleetAutoscalersOf returns the FleetAutoscaler view of s.
func FleetAutoscalersOf(s *Store) Typed[*autoscalingv1.FleetAutoscaler] {
	return Typed[*autoscalingv1.FleetAutoscaler]{store: s, kind: FleetAutoscalers}
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
	return typed[T](t.store.List(t.kind, namespace))
}

// ListAndWatch is List and Watch in one step: the events start exactly
// where the list ends.
func (t Typed[T]) ListAndWatch(namespace string) (objects []T, events <-chan Event, cancel func()) {
	objs, events, cancel := t.store.ListAndWatch(t.kind, namespace)
	return typed[T](objs), events, cancel
}

func typed[T Object](objs []Object) []T {
	out := make([]T, len(objs))
	for i, obj := range objs {
		out[i] = obj.(T)
	}
	return out
}

// View returns every object in namespace, or in all when it is empty,
// without copying. The objects are shared: read them, never change them.
// See Store.View.
func (t Typed[T]) View(namespace string) []T {
	objs := t.store.View(t.kind, namespace)
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

// Change reads the named object, lets change alter it, and stores the result.
// It starts over when someone else updated the object in between, so change
// may run more than once. A missing object is not an error: there is nothing
// to change.
func (t Typed[T]) Change(namespace, name string, change func(T) (keep bool)) error {
	const attempts = 5
	var err error
	for range attempts {
		obj, ok := t.Get(namespace, name)
		if !ok || !change(obj) {
			return nil
		}
		if _, err = t.Update(obj); !errors.Is(err, ErrConflict) {
			return err
		}
	}
	return err
}

// MarkDeleted records that the object should go away. Its controller cleans
// up what belongs to it and then calls Delete. This is the job a finalizer
// does in Kubernetes.
func (t Typed[T]) MarkDeleted(namespace, name string, now time.Time) error {
	return t.Change(namespace, name, func(obj T) bool {
		if obj.GetDeletionTimestamp() != nil {
			return false
		}
		stamp := metav1.NewTime(now)
		obj.SetDeletionTimestamp(&stamp)
		return true
	})
}

// Now tells the time by the store's clock.
func (t Typed[T]) Now() time.Time {
	return t.store.Now()
}

// Sync returns when the file has every change made so far, to any kind.
// See Store.Run for who has to call it.
func (t Typed[T]) Sync() error {
	return t.store.Sync()
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
