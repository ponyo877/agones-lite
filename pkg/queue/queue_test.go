package queue

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestAKeyWaitsOnlyOnce(t *testing.T) {
	q := New[string]()
	q.Add("a")
	q.Add("a")
	q.Add("b")

	ctx, cancel := context.WithCancel(context.Background())
	var handled []string
	q.Run(ctx, func(_ context.Context, key string) error {
		handled = append(handled, key)
		if len(handled) == 2 {
			cancel()
		}
		return nil
	})

	assert.Equal(t, []string{"a", "b"}, handled)
}

func TestAFailedKeyComesBack(t *testing.T) {
	q := New[string]()
	q.Add("a")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	attempts := 0
	q.Run(ctx, func(_ context.Context, _ string) error {
		attempts++
		if attempts < 3 {
			return errors.New("not yet")
		}
		cancel()
		return nil
	})

	assert.Equal(t, 3, attempts)
}

func TestBackoffGrowsThenStops(t *testing.T) {
	assert.Equal(t, 10*time.Millisecond, Backoff(1))
	assert.Equal(t, 20*time.Millisecond, Backoff(2))
	assert.Equal(t, 10*time.Second, Backoff(50))
}
