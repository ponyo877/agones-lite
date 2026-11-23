package store

// EventType says what happened to an object.
type EventType string

// The three things that can happen to an object.
const (
	Added    EventType = "Added"
	Modified EventType = "Modified"
	Deleted  EventType = "Deleted"
)

// Event is one change. For Deleted, Object is the last stored version.
// Object is shared with the store and must not be changed.
type Event struct {
	Type   EventType
	Key    Key
	Object Object
}

// watchBuffer is how far a watcher may fall behind before it loses events.
const watchBuffer = 1024

type watcher struct {
	kind   Kind
	events chan Event
}

// Watch delivers every change to kind until cancel is called.
//
// A slow watcher loses events rather than blocking writers. Controllers cope
// with that by also listing everything from time to time.
func (s *Store) Watch(kind Kind) (events <-chan Event, cancel func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.subscribe(kind)
}

// ListAndWatch is List and Watch in one step. The events start exactly
// where the list ends, so no change is missed and none is told twice.
func (s *Store) ListAndWatch(kind Kind, namespace string) (objects []Object, events <-chan Event, cancel func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	events, cancel = s.subscribe(kind)
	return s.list(kind, namespace), events, cancel
}

// subscribe is called with the lock held.
func (s *Store) subscribe(kind Kind) (events <-chan Event, cancel func()) {
	w := &watcher{kind: kind, events: make(chan Event, watchBuffer)}
	s.watchers[w] = struct{}{}
	return w.events, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if _, ok := s.watchers[w]; ok {
			delete(s.watchers, w)
			close(w.events)
		}
	}
}

// notify is called with the lock held.
func (s *Store) notify(e Event) {
	for w := range s.watchers {
		if w.kind != e.Key.Kind {
			continue
		}
		// Like View, an event carries the stored object itself: read only.
		select {
		case w.events <- e:
		default: // the watcher is too far behind
		}
	}
}
