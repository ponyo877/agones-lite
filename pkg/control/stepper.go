package control

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/ponyo877/scrim/pkg/store"
)

// Stepper drives Loops by hand: one Sync at a time, in an order its caller
// picks.
//
// Run leaves the order to goroutines and timers. That is right for a server
// and useless for finding the order that breaks something, because a test
// cannot ask for the same order again. With a Stepper the order is data: a
// test draws it at random, and replays it when it fails.
//
// Nothing happens unless Run is called, so it is of no use outside tests.
type Stepper struct {
	loops []*stepped
}

// Step is one Sync that waits to be run.
type Step struct {
	Loop string
	Key  store.Key
}

func (s Step) String() string {
	return fmt.Sprintf("%s looks at %s", s.Loop, s.Key)
}

// stepped is a Loop with its queue, which here is only a set.
type stepped struct {
	name    string
	loop    Loop
	feeds   []*feed
	waiting map[store.Key]bool
}

// feed is one subscription of a loop.
type feed struct {
	watch  Watch
	events <-chan store.Event
	cancel func()
}

// Take puts a controller under the Stepper's control, in place of Run.
func (s *Stepper) Take(name string, c Controller) {
	loop, watches := c.Loop()
	s.Add(name, loop, watches...)
}

// Add puts a loop under the Stepper's control. Like Loop.Run, it begins by
// queueing everything.
func (s *Stepper) Add(name string, l Loop, watches ...Watch) {
	loop := &stepped{name: name, loop: l, waiting: map[store.Key]bool{}}
	for _, w := range watches {
		events, cancel := w.Subscribe()
		loop.feeds = append(loop.feeds, &feed{watch: w, events: events, cancel: cancel})
	}
	loop.addAll()
	s.loops = append(s.loops, loop)
}

// Pending lists the Syncs that wait. Any of them may run next. The order of
// the list depends on nothing but its contents, so a choice made by position
// can be made again.
func (s *Stepper) Pending() []Step {
	var steps []Step
	for _, loop := range s.loops {
		loop.hear()
		keys := slices.SortedFunc(maps.Keys(loop.waiting), func(a, b store.Key) int {
			return cmp.Compare(a.String(), b.String())
		})
		for _, key := range keys {
			steps = append(steps, Step{Loop: loop.name, Key: key})
		}
	}
	return steps
}

// Run carries out one Step. A Sync that fails waits again, as it would
// after a delay under Loop.Run; its error is returned for the test to see.
func (s *Stepper) Run(ctx context.Context, step Step) error {
	for _, loop := range s.loops {
		if loop.name != step.Loop {
			continue
		}
		delete(loop.waiting, step.Key)
		err := loop.loop.Sync(ctx, step.Key)
		if err != nil {
			loop.waiting[step.Key] = true
		}
		return err
	}
	return fmt.Errorf("no loop is called %q", step.Loop)
}

// Resync queues everything, as every Loop does for itself each Resync.
func (s *Stepper) Resync() {
	for _, loop := range s.loops {
		loop.addAll()
	}
}

// Close ends the subscriptions.
func (s *Stepper) Close() {
	for _, loop := range s.loops {
		for _, f := range loop.feeds {
			f.cancel()
		}
	}
}

// hear queues what the changes since the last call ask for.
func (l *stepped) hear() {
	for _, f := range l.feeds {
		for l.heard(f) {
		}
	}
}

// heard takes one change from the feed, if one waits there.
func (l *stepped) heard(f *feed) bool {
	select {
	case e, open := <-f.events:
		if !open {
			// The store gave up on us: see Loop.follow.
			f.events, f.cancel = f.watch.Subscribe()
			l.addAll()
			return true
		}
		l.add(f.watch.Keys, e)
		return true
	default:
		return false
	}
}

func (l *stepped) add(keys func(store.Event) []store.Key, e store.Event) {
	if keys == nil {
		l.addAll()
		return
	}
	for _, key := range keys(e) {
		l.waiting[key] = true
	}
}

func (l *stepped) addAll() {
	for _, key := range l.loop.All() {
		l.waiting[key] = true
	}
}
