package control

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ponyo877/scrim/pkg/store"
)

func named(name string) store.Key {
	return store.Key{Kind: store.GameServers, Namespace: "default", Name: name}
}

// next returns the key the loop looks at next.
func next(t *testing.T, synced <-chan store.Key) store.Key {
	t.Helper()
	select {
	case key := <-synced:
		return key
	case <-time.After(5 * time.Second):
		require.Fail(t, "the loop looked at nothing")
		return store.Key{}
	}
}

func TestALoopLooksAtEverythingWhenItStarts(t *testing.T) {
	synced := make(chan store.Key)
	loop := Loop{
		Sync:   func(_ context.Context, key store.Key) error { synced <- key; return nil },
		All:    func() []store.Key { return []store.Key{named("there-already")} },
		Resync: time.Hour,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go loop.Run(ctx) // with nothing to follow: nothing else would tell it

	assert.Equal(t, named("there-already"), next(t, synced))
}

func TestALoopLooksAtWhatChanged(t *testing.T) {
	events := make(chan store.Event)
	synced := make(chan store.Key)
	loop := Loop{
		Sync:   func(_ context.Context, key store.Key) error { synced <- key; return nil },
		All:    func() []store.Key { return nil },
		Resync: time.Hour,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go loop.Run(ctx, Watch{Subscribe: func() (<-chan store.Event, func()) { return events, func() {} }, Keys: Self})

	events <- store.Event{Type: store.Modified, Key: named("changed")}

	assert.Equal(t, named("changed"), next(t, synced))
}

func TestALoopWhoseSubscriptionEndedSubscribesAgainAndLooksAtEverything(t *testing.T) {
	first, second := make(chan store.Event), make(chan store.Event)
	subscriptions := make(chan chan store.Event, 2)
	subscriptions <- first
	subscriptions <- second
	var missed atomic.Bool // a change the loop was never told of
	synced := make(chan store.Key)
	loop := Loop{
		Sync: func(_ context.Context, key store.Key) error { synced <- key; return nil },
		All: func() []store.Key {
			if missed.Load() {
				return []store.Key{named("missed")}
			}
			return nil
		},
		Resync: time.Hour,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go loop.Run(ctx, Watch{Subscribe: func() (<-chan store.Event, func()) { return <-subscriptions, func() {} }, Keys: Self})
	first <- store.Event{Type: store.Modified, Key: named("seen")}
	require.Equal(t, named("seen"), next(t, synced))

	// What the store does to a watcher that fell too far behind.
	missed.Store(true)
	close(first)

	assert.Equal(t, named("missed"), next(t, synced), "the lost changes may concern anything")
	second <- store.Event{Type: store.Modified, Key: named("later")}
	assert.Equal(t, named("later"), next(t, synced), "and changes are followed again")
}
