package sdkserver

import (
	"context"
	"testing"
	"time"

	"agones.dev/agones/pkg/sdk"
	sdkbeta "agones.dev/agones/pkg/sdk/beta"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	sidecarv1 "github.com/ponyo877/scrim/proto/sidecar/v1"
)

// increment asks for one more player through the SDK.
func increment(t *testing.T, s *Server) {
	t.Helper()
	_, err := beta{server: s}.UpdateCounter(context.Background(), &sdkbeta.UpdateCounterRequest{
		CounterUpdateRequest: &sdkbeta.CounterUpdateRequest{Name: "players", CountDiff: 1},
	})
	require.NoError(t, err)
}

// shown returns what GetGameServer answers.
func shown(t *testing.T, s *Server) *sdk.GameServer {
	t.Helper()
	gs, err := s.GetGameServer(context.Background(), &sdk.Empty{})
	require.NoError(t, err)
	return gs
}

// waiting returns how many changes the agent has not answered yet.
func waiting(s *Server) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pending)
}

func TestAReadRightAfterAWriteShowsTheNewValue(t *testing.T) {
	base := withCounter("5", counter{10, 100})
	s := New(&fakeAgent{}) // nothing is delivered, so the change stays pending
	s.set(base)

	increment(t, s)

	assert.Equal(t, counter{11, 100}, playersIn(shown(t, s)), "GetGameServer shows the change too")
	assert.Equal(t, counter{10, 100}, playersIn(base), "what the agent sent is left as it was")
}

func TestWatchersSeeAChangeAsSoonAsItIsMade(t *testing.T) {
	s := New(&fakeAgent{})
	s.set(withCounter("5", counter{10, 100}))
	updates, cancel := s.subscribe()
	defer cancel()
	assert.Equal(t, counter{10, 100}, playersIn(<-updates))

	increment(t, s)

	select {
	case gs := <-updates:
		assert.Equal(t, counter{11, 100}, playersIn(gs))
	case <-time.After(time.Second):
		assert.Fail(t, "the watcher was not told")
	}
}

func TestWatchersAreNotToldTheSameThingTwice(t *testing.T) {
	s := New(&fakeAgent{})
	s.set(withCounter("5", counter{10, 100}))
	updates, cancel := s.subscribe()
	defer cancel()
	<-updates

	s.set(withCounter("5", counter{10, 100})) // the agent sends it again after reconnecting

	select {
	case <-updates:
		assert.Fail(t, "nothing changed")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestTheAgentsAnswerBecomesTheBase(t *testing.T) {
	gs := withCounter("5", counter{10, 100})
	s := New(&fakeAgent{stored: gs})
	s.set(gs)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.deliver(ctx)

	increment(t, s)

	require.Eventually(t, func() bool { return waiting(s) == 0 }, 2*time.Second, 10*time.Millisecond)
	view := shown(t, s)
	assert.Equal(t, "6", view.GetObjectMeta().GetResourceVersion(), "this is the GameServer the agent answered with")
	assert.Equal(t, counter{11, 100}, playersIn(view))
}

func TestAChangeStaysVisibleWhileTheAgentIsAway(t *testing.T) {
	gs := withCounter("5", counter{10, 100})
	agent := &fakeAgent{stored: gs, refusals: 1}
	s := New(agent)
	s.set(gs)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.deliver(ctx)

	increment(t, s)
	assert.Equal(t, counter{11, 100}, playersIn(shown(t, s)))

	require.Eventually(t, func() bool { return waiting(s) == 0 }, 3*retryDelay, 10*time.Millisecond)
	assert.Len(t, agent.events(), 1, "the agent took the change once")
	assert.Equal(t, counter{11, 100}, playersIn(shown(t, s)))
}

func TestAChangeThatCannotBeQueuedIsNotMade(t *testing.T) {
	s := New(&fakeAgent{})
	s.set(withCounter("5", counter{10, 100}))
	for range cap(s.outbox) {
		_, err := s.Ready(context.Background(), &sdk.Empty{})
		require.NoError(t, err)
	}

	_, err := beta{server: s}.UpdateCounter(context.Background(), &sdkbeta.UpdateCounterRequest{
		CounterUpdateRequest: &sdkbeta.CounterUpdateRequest{Name: "players", CountDiff: 1},
	})

	assert.Equal(t, codes.Unavailable, status.Code(err))
	assert.Equal(t, counter{10, 100}, playersIn(shown(t, s)))
}

func TestALatePushCannotUndoANewerGameServer(t *testing.T) {
	s := New(&fakeAgent{})
	s.set(withCounter("6", counter{11, 100}))

	s.set(withCounter("5", counter{10, 100}))

	assert.Equal(t, "6", shown(t, s).GetObjectMeta().GetResourceVersion())
}

// The agent stores a change and then does two things in no fixed order: it
// answers the sidecar, and it pushes the changed GameServer to it.
func TestAPushThatOvertakesTheAnswerIsNotCountedTwice(t *testing.T) {
	s := New(&fakeAgent{})
	s.set(withCounter("5", counter{10, 100}))
	increment(t, s)
	change := s.pending[0]
	after := withCounter("6", counter{11, 100}) // the GameServer with the change stored

	s.set(after)
	assert.Equal(t, counter{11, 100}, playersIn(shown(t, s)), "the push waits: the pending change is not made on top of it")

	s.settle(change, after)
	assert.Equal(t, counter{11, 100}, playersIn(shown(t, s)))
	assert.Equal(t, "6", shown(t, s).GetObjectMeta().GetResourceVersion())
	assert.Zero(t, waiting(s))
}

func TestWhatWasPushedDuringAChangeIsShownOnceTheAnswerIsIn(t *testing.T) {
	tests := []struct {
		name   string
		pushed string // the version pushed while the change was pending
		answer string // the version the agent answered the change with
		want   string
	}{
		{name: "the push is newer than the answer", pushed: "7", answer: "6", want: "7"},
		{name: "the push is older than the answer", pushed: "5", answer: "6", want: "6"},
		{name: "the agent answers without a GameServer", pushed: "7", answer: "", want: "7"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := New(&fakeAgent{})
			s.set(withCounter("4", counter{10, 100}))
			increment(t, s)
			change := s.pending[0]

			s.set(withCounter(tc.pushed, counter{11, 100}))
			assert.Equal(t, "4", shown(t, s).GetObjectMeta().GetResourceVersion(), "held back for now")

			var answer *sdk.GameServer
			if tc.answer != "" {
				answer = withCounter(tc.answer, counter{11, 100})
			}
			s.settle(change, answer)
			assert.Equal(t, tc.want, shown(t, s).GetObjectMeta().GetResourceVersion())
		})
	}
}

func TestAnyAnswerLetsAHeldPushThrough(t *testing.T) {
	s := New(&fakeAgent{})
	s.set(withCounter("5", counter{10, 100}))
	ready := &sidecarv1.TellRequest{Event: sidecarv1.Event_EVENT_READY}
	increment(t, s)
	s.set(withCounter("6", counter{10, 100})) // say, the state changed

	// The agent answers a request that went out before the change. It has
	// not seen the change yet, so neither has what it pushed.
	s.settle(ready, nil)

	view := shown(t, s)
	assert.Equal(t, "6", view.GetObjectMeta().GetResourceVersion())
	assert.Equal(t, counter{11, 100}, playersIn(view), "the change is still pending, on top of the new base")
	assert.Equal(t, 1, waiting(s))
}

func TestOlderComparesVersionsAsNumbers(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"5", "6", true},
		{"6", "5", false},
		{"6", "6", false},
		{"9", "10", true}, // not as text
		{"10", "9", false},
		{"", "6", false},
		{"5", "", false},
		{"five", "6", false},
	}
	for _, tc := range tests {
		t.Run(tc.a+" before "+tc.b, func(t *testing.T) {
			a := &sdk.GameServer{ObjectMeta: &sdk.GameServer_ObjectMeta{ResourceVersion: tc.a}}
			b := &sdk.GameServer{ObjectMeta: &sdk.GameServer_ObjectMeta{ResourceVersion: tc.b}}
			assert.Equal(t, tc.want, older(a, b))
		})
	}
	assert.False(t, older(&sdk.GameServer{}, nil), "nothing is older than no GameServer at all")
}
