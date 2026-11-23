// Package control holds the loop that every controller runs.
//
// A controller is two things. One is a judgement about a single resource: a
// pure function that lives in the controller's own package. The other is
// deciding when to make that judgement again, which is the same for all of
// them and lives here: after a change that may matter, and for everything
// now and then, in case a change was missed.
//
// Upstream counterpart: the informer event handlers and worker queues that
// each Agones controller sets up in its NewController.
package control

import (
	"context"
	"time"

	"github.com/ponyo877/agones-lite/pkg/queue"
	"github.com/ponyo877/agones-lite/pkg/store"
)

// Watch is one kind of resource a controller wants to hear about.
type Watch struct {
	// Subscribe starts delivering the changes of that kind.
	Subscribe func() (events <-chan store.Event, cancel func())
	// Keys names what has to be looked at again after a change. Leave it
	// nil when a change may concern everything the controller owns.
	Keys func(store.Event) []store.Key
}

// Self is the Keys function for "a change concerns the changed resource".
func Self(e store.Event) []store.Key {
	return []store.Key{e.Key}
}

// Loop looks at resources one at a time, whenever they may need attention.
type Loop struct {
	// Sync looks at one resource and does what it needs. An error means
	// "try again later".
	Sync func(ctx context.Context, key store.Key) error
	// All lists every resource the controller is responsible for.
	All func() []store.Key
	// Resync is how often everything is looked at again.
	Resync time.Duration
}

// Run works until ctx is done.
func (l Loop) Run(ctx context.Context, watches ...Watch) {
	q := queue.New[store.Key]()
	for _, w := range watches {
		events, cancel := w.Subscribe()
		defer cancel()
		go l.feed(ctx, q, events, w.Keys)
	}
	go l.resync(ctx, q)
	q.Run(ctx, l.Sync)
}

// feed turns the changes of one kind into queue entries.
func (l Loop) feed(ctx context.Context, q *queue.Queue[store.Key], events <-chan store.Event, keys func(store.Event) []store.Key) {
	for {
		select {
		case <-ctx.Done():
			return
		case e, open := <-events:
			if !open {
				return
			}
			l.add(q, e, keys)
		}
	}
}

func (l Loop) add(q *queue.Queue[store.Key], e store.Event, keys func(store.Event) []store.Key) {
	if keys == nil {
		l.addAll(q)
		return
	}
	for _, key := range keys(e) {
		q.Add(key)
	}
}

func (l Loop) addAll(q *queue.Queue[store.Key]) {
	for _, key := range l.All() {
		q.Add(key)
	}
}

// resync queues everything at the start and then every Resync.
func (l Loop) resync(ctx context.Context, q *queue.Queue[store.Key]) {
	tick := time.NewTicker(l.Resync)
	defer tick.Stop()
	for {
		l.addAll(q)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
