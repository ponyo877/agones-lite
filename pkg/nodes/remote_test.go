package nodes

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/ponyo877/agones-lite/pkg/auth"
)

// logBook collects what is logged while a test runs.
type logBook struct {
	mu   sync.Mutex
	text strings.Builder
}

func (l *logBook) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.text.Write(p)
}

// count returns how many times phrase was logged.
func (l *logBook) count(phrase string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Count(l.text.String(), phrase)
}

func logged(t *testing.T) *logBook {
	t.Helper()
	book, before := &logBook{}, slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(book, nil)))
	t.Cleanup(func() { slog.SetDefault(before) })
	return book
}

const (
	remedy      = "agones-lite token show join"
	reconnected = "lost the control plane, reconnecting"
)

func TestARefusedJoinTokenIsSaidOnceAndAgainOnlyAfterSomethingElseHappened(t *testing.T) {
	log := logged(t)
	refusal := status.Error(codes.Unauthenticated, "a valid token is required")
	outage := status.Error(codes.Unavailable, "connection refused")

	// The token was rotated, and the agent keeps trying every two seconds.
	refused := false
	for range 5 {
		refused = explain(refusal, refused)
	}
	assert.Equal(t, 1, log.count(remedy), "nothing changes by itself, so saying it again is noise")
	assert.Zero(t, log.count(reconnected), "it is not the connection that is lost")

	// The control plane goes away and comes back, still with the new token.
	refused = explain(outage, refused)
	assert.Equal(t, 1, log.count(reconnected))
	explain(refusal, refused)
	assert.Equal(t, 2, log.count(remedy), "after something else happened it is worth saying again")
}

func TestAnAgentWithARefusedJoinTokenIsToldWhereTheNewOneComesFrom(t *testing.T) {
	cp := newControlPlane(t)
	log := logged(t)
	stale := cp.secrets.Token(auth.Node)
	require.NoError(t, cp.secrets.Rotate(auth.Node))
	remote, err := Dial(cp.address, stale)
	require.NoError(t, err)
	t.Cleanup(func() { _ = remote.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go remote.Run(ctx, "node-1")

	soon(t, func() bool { return log.count(remedy) == 1 }, "the first refusal is explained")
	again := func() bool { return log.count(remedy) > 1 || log.count(reconnected) > 0 }
	assert.Never(t, again, reconnectDelay+time.Second, 50*time.Millisecond, "the next attempts are refused in silence")
}
