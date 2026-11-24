// Package queue is a small work queue for controllers.
//
// A controller adds the key of every resource that may need attention and
// handles keys one at a time. Two properties keep that simple:
//
//   - A key that is already waiting is not added twice, so a burst of changes
//     to one resource costs a single pass.
//   - A key whose handler fails comes back after a delay that grows with each
//     failure.
//
// Upstream counterpart: pkg/util/workerqueue, built on client-go's workqueue.
package queue

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Queue holds keys of type K. The zero value is not usable; call New.
type Queue[K comparable] struct {
	mu       sync.Mutex
	waiting  map[K]struct{}
	order    []K
	failures map[K]int
	wake     chan struct{}
}

// New returns an empty queue.
func New[K comparable]() *Queue[K] {
	return &Queue[K]{
		waiting:  map[K]struct{}{},
		failures: map[K]int{},
		wake:     make(chan struct{}, 1),
	}
}

// Add schedules key. It does nothing when key is already waiting.
func (q *Queue[K]) Add(key K) {
	q.mu.Lock()
	if _, ok := q.waiting[key]; !ok {
		q.waiting[key] = struct{}{}
		q.order = append(q.order, key)
	}
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default: // a wake-up is already pending
	}
}

// AddAfter schedules key once d has passed.
func (q *Queue[K]) AddAfter(key K, d time.Duration) {
	time.AfterFunc(d, func() { q.Add(key) })
}

// Run calls handle for one key at a time until ctx is done.
func (q *Queue[K]) Run(ctx context.Context, handle func(context.Context, K) error) {
	for {
		key, ok := q.next(ctx)
		if !ok {
			return
		}
		q.finish(key, handle(ctx, key))
	}
}

// next blocks until a key is waiting or ctx is done.
func (q *Queue[K]) next(ctx context.Context) (K, bool) {
	for {
		if key, ok := q.pop(); ok {
			return key, true
		}
		select {
		case <-ctx.Done():
			var zero K
			return zero, false
		case <-q.wake:
		}
	}
}

func (q *Queue[K]) pop() (K, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.order) == 0 {
		var zero K
		return zero, false
	}
	key := q.order[0]
	q.order = q.order[1:]
	delete(q.waiting, key)
	return key, true
}

// finish forgets a key that succeeded and reschedules one that failed.
func (q *Queue[K]) finish(key K, err error) {
	q.mu.Lock()
	if err == nil {
		delete(q.failures, key)
		q.mu.Unlock()
		return
	}
	q.failures[key]++
	delay := Backoff(q.failures[key])
	q.mu.Unlock()
	slog.Warn("will retry", "key", key, "after", delay, "error", err)
	q.AddAfter(key, delay)
}

// Backoff is the delay before retry number failures: 10ms, doubling, up to 10s.
func Backoff(failures int) time.Duration {
	const first, limit = 10 * time.Millisecond, 10 * time.Second
	delay := first
	for i := 1; i < failures && delay < limit; i++ {
		delay *= 2
	}
	return min(delay, limit)
}
