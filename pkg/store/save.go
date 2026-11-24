package store

import (
	"context"
	"log/slog"
	"slices"
	"time"
)

// change is one write: made in memory, and on its way to the file.
type change struct {
	event    Event // what watchers are told once it is saved
	revision int64
	was      Object // what the key held before, or nil: the way back
	data     []byte // the object as the file stores it; nil for a deletion
}

// after is what the key holds once the change is made, or nil.
func (c change) after() Object {
	if c.event.Type == Deleted {
		return nil
	}
	return c.event.Object
}

// retryAfter is the pause before Run tries again to save.
const retryAfter = time.Second

// Run saves changes to the file in the background until ctx is done.
//
// Without it, a write returns once the file has it, and every write pays
// for its own sync of the disk: a millisecond or two, during which nobody
// else can write. While Run is running, a write changes memory and returns.
// Run puts whatever has gathered into the file in one transaction, with one
// sync, and starts on the next round. Controllers that write a thousand
// times in a row no longer wait a thousand times.
//
// What is given up is narrow. Reads show changes that are not in the file
// yet, and a crash loses those. So whoever tells the outside world about a
// change calls Sync first: an allocation is answered, a request to the API
// is answered, an agent hears of a report having arrived, only when the
// file has what the answer rests on. Watchers need not: they are told of a
// change once it is saved, in the order the changes were made. A crash can
// only take back what nobody outside was told.
//
// A save that fails is tried again until it works. Until then nothing new
// is promised to anyone: Sync waits.
func (s *Store) Run(ctx context.Context) {
	s.setBehind(true)
	for s.unsavedChanges(ctx) {
		if err := s.saveBehind(); err != nil {
			slog.Error("cannot save to the file; trying again", "after", retryAfter, "error", err)
			s.pause(ctx)
		}
	}
	s.setBehind(false)
}

// Sync returns when the file has every change made before the call.
func (s *Store) Sync() error {
	s.mu.Lock()
	upTo := s.revision
	s.mu.Unlock()

	s.waiting.Lock()
	for s.saved.Load() < upTo && s.behind.Load() {
		s.settled.Wait()
	}
	s.waiting.Unlock()
	if s.saved.Load() >= upTo {
		return nil
	}
	// Run has ended and left it to us.
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.save()
}

// tellWaiting wakes those who wait in Sync, to look again.
func (s *Store) tellWaiting() {
	s.waiting.Lock()
	s.settled.Broadcast()
	s.waiting.Unlock()
}

// record makes a change in memory and sends it on its way to the file.
// Called with the lock held.
//
// When nobody saves in the background, the write saves itself before the
// lock is released, and a change the file refuses is taken back: to
// everyone else it never happened.
func (s *Store) record(c change) error {
	s.hold(c.event.Key, c.after())
	s.revision = c.revision
	s.unsaved = append(s.unsaved, c)
	if s.behind.Load() {
		s.nudge()
		return nil
	}
	err := s.save()
	if err != nil {
		s.hold(c.event.Key, c.was)
		s.revision = c.revision - 1
		s.unsaved = s.unsaved[:len(s.unsaved)-1]
	}
	return err
}

// save writes every unsaved change to the file. Called with the lock held,
// and keeps it: so it does not wait for a watcher that has no room.
func (s *Store) save() error {
	if len(s.unsaved) == 0 {
		return nil
	}
	if err := s.file.write(s.unsaved); err != nil {
		return err
	}
	saved := s.settle(len(s.unsaved))
	for _, w := range s.listening() {
		if !tellAll(w, saved, 0) {
			s.drop(w) // too far behind: see Watch
		}
	}
	return nil
}

// saveBehind writes what is unsaved while writers carry on: the lock is
// not held during the write. What they add meanwhile is for the next round.
func (s *Store) saveBehind() error {
	s.mu.Lock()
	round := s.unsaved
	s.mu.Unlock()
	if len(round) == 0 {
		return nil
	}
	if err := s.file.write(round); err != nil {
		return err
	}
	s.mu.Lock()
	saved, listening := s.settle(len(round)), s.listening()
	s.mu.Unlock()
	// Without the lock: a watcher gets a moment to make room, and nobody
	// who writes or reads is kept waiting for it meanwhile.
	for _, w := range listening {
		if !tellAll(w, saved, patience) {
			s.mu.Lock()
			s.drop(w) // stuck: see Watch
			s.mu.Unlock()
		}
	}
	return nil
}

// settle is called with the lock held once the file has the first n
// unsaved changes. Sync may return, and what it returns is for the
// watchers to hear of.
func (s *Store) settle(n int) (saved []change) {
	saved = s.unsaved[:n:n]
	s.saved.Store(saved[n-1].revision)
	s.unsaved = slices.Clone(s.unsaved[n:]) // a copy, to let go of the saved ones
	s.tellWaiting()
	return saved
}

// tellAll tells a watcher of changes, in order, and reports false when it
// could not keep up.
func tellAll(w *watcher, changes []change, wait time.Duration) (kept bool) {
	for _, c := range changes {
		if !w.tell(c, wait) {
			return false
		}
	}
	return true
}

// setBehind says whether Run is saving. When it stops, what it has left is
// saved here, or handed to the next write or Sync if that fails too.
func (s *Store) setBehind(behind bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.behind.Store(behind)
	if behind {
		return
	}
	if err := s.save(); err != nil {
		slog.Error("changes are left unsaved", "changes", len(s.unsaved), "error", err)
	}
	s.tellWaiting()
}

// nudge tells Run there is something to save.
func (s *Store) nudge() {
	select {
	case s.wake <- struct{}{}:
	default: // it has been told already
	}
}

// unsavedChanges waits until there is something to save, and reports false
// when ctx is done instead.
func (s *Store) unsavedChanges(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return false
	case <-s.wake:
		return true
	}
}

// pause waits a little before a save is tried again, and makes sure that
// it is.
func (s *Store) pause(ctx context.Context) {
	select {
	case <-ctx.Done():
	case <-time.After(retryAfter):
	}
	s.nudge()
}
