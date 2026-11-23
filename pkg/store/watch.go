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
	w := &watcher{kind: kind, events: make(chan Event, watchBuffer)}
	s.mu.Lock()
	s.watchers[w] = struct{}{}
	s.mu.Unlock()
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
		copied := Event{Type: e.Type, Key: e.Key, Object: clone(e.Object)}
		select {
		case w.events <- copied:
		default: // the watcher is too far behind
		}
	}
}
