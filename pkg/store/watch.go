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

// watchBuffer is how far a watcher may fall behind before it is closed.
const watchBuffer = 1024

type watcher struct {
	kind   Kind
	events chan Event
	after  int64 // the revision it began at: it is told what came later
}

// Watch delivers every change to kind made from now on, until cancel is
// called. A change is delivered once the file has it: see Run.
//
// Writers never wait for a watcher. One that falls watchBuffer changes
// behind is closed instead: it can still read what it was sent, and then
// its channel ends. Skipping changes would leave it with a copy that looks
// whole and is not, so it has to notice, and start again from a fresh list.
// Calling cancel afterwards is harmless.
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
	w := &watcher{kind: kind, events: make(chan Event, watchBuffer), after: s.revision}
	s.watchers[w] = struct{}{}
	return w.events, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.drop(w)
	}
}

// drop ends a watcher, unless that was done before. Called with the lock held.
func (s *Store) drop(w *watcher) {
	if _, ok := s.watchers[w]; ok {
		delete(s.watchers, w)
		close(w.events)
	}
}

// notify tells the watchers of a change. Called with the lock held.
func (s *Store) notify(c change) {
	for w := range s.watchers {
		if w.kind != c.event.Key.Kind || c.revision <= w.after {
			continue
		}
		// Like View, an event carries the stored object itself: read only.
		select {
		case w.events <- c.event:
		default:
			s.drop(w) // too far behind: see Watch
		}
	}
}
