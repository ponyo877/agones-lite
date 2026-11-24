package store

import (
	"maps"
	"slices"
	"sync"
	"time"
)

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

// patience is how long Run waits for a watcher to make room. The changes
// of one save reach a watcher all at once, and may be more than it has
// room for: a watcher that is reading catches up in no time, and only one
// that is stuck is still full after this long.
const patience = time.Second

type watcher struct {
	kind   Kind
	events chan Event
	after  int64 // the revision it began at: it is told what came later
	// Run tells watchers without the store's lock. gone is closed when the
	// watcher ends, so that a send which waits for room gives up at once,
	// and sending keeps the channel from being closed under a send.
	gone    chan struct{}
	sending sync.Mutex
}

// Watch delivers every change to kind made from now on, until cancel is
// called. A change is delivered once the file has it: see Run.
//
// Writers never wait for a watcher. One that falls watchBuffer changes
// behind is closed instead: it can still read what it was sent, and then
// its channel ends. Skipping changes would leave it with a copy that looks
// whole and is not, so it has to notice, and start again from a fresh list.
// Calling cancel afterwards is harmless.
//
// Run, which writes the file behind, does wait: for a moment, see patience.
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
	w := &watcher{kind: kind, events: make(chan Event, watchBuffer), after: s.revision, gone: make(chan struct{})}
	s.watchers[w] = struct{}{}
	return w.events, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.drop(w)
	}
}

// drop ends a watcher, unless that was done before. Called with the lock held.
func (s *Store) drop(w *watcher) {
	if _, ok := s.watchers[w]; !ok {
		return
	}
	delete(s.watchers, w)
	close(w.gone) // a send that waits for room returns now
	w.sending.Lock()
	close(w.events)
	w.sending.Unlock()
}

// tell sends a watcher one change, if it concerns it, and reports false
// when the watcher has no room for it, and made none while wait lasted.
func (w *watcher) tell(c change, wait time.Duration) (kept bool) {
	if w.kind != c.event.Key.Kind || c.revision <= w.after {
		return true
	}
	w.sending.Lock()
	defer w.sending.Unlock()
	select {
	case <-w.gone:
		return true // it has left, and its channel is closed
	default:
	}
	// Like View, an event carries the stored object itself: read only.
	select {
	case w.events <- c.event:
		return true
	default:
	}
	if wait == 0 {
		return false
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-w.gone:
		return true
	case w.events <- c.event:
		return true
	case <-timer.C:
		return false
	}
}

// listening returns who watches now. Called with the lock held.
func (s *Store) listening() []*watcher {
	return slices.Collect(maps.Keys(s.watchers))
}
