package sdkserver

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"agones.dev/agones/pkg/sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	sidecarv1 "github.com/ponyo877/agones-lite/proto/sidecar/v1"
)

// fakeAgent records what the sidecar tells the agent. It can be told to
// refuse the first few requests, like an agent that is restarting.
type fakeAgent struct {
	mu       sync.Mutex
	told     []*sidecarv1.TellRequest
	refusals int
}

func (f *fakeAgent) Tell(_ context.Context, req *sidecarv1.TellRequest, _ ...grpc.CallOption) (*sidecarv1.TellResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.refusals > 0 {
		f.refusals--
		return nil, errors.New("agent is restarting")
	}
	f.told = append(f.told, req)
	return &sidecarv1.TellResponse{}, nil
}

func (f *fakeAgent) Watch(context.Context, *sidecarv1.WatchRequest, ...grpc.CallOption) (grpc.ServerStreamingClient[sidecarv1.WatchResponse], error) {
	return nil, errors.New("not used in these tests")
}

func (f *fakeAgent) events() []sidecarv1.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []sidecarv1.Event
	for _, req := range f.told {
		out = append(out, req.GetEvent())
	}
	return out
}

func TestRequestsReachTheAgentInOrder(t *testing.T) {
	agent := &fakeAgent{}
	s := New(agent)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.deliver(ctx)

	_, err := s.Ready(ctx, &sdk.Empty{})
	require.NoError(t, err)
	_, err = s.Allocate(ctx, &sdk.Empty{})
	require.NoError(t, err)
	_, err = s.Shutdown(ctx, &sdk.Empty{})
	require.NoError(t, err)

	want := []sidecarv1.Event{sidecarv1.Event_EVENT_READY, sidecarv1.Event_EVENT_ALLOCATE, sidecarv1.Event_EVENT_SHUTDOWN}
	assert.Eventually(t, func() bool { return assert.ObjectsAreEqual(want, agent.events()) }, 2*time.Second, 10*time.Millisecond)
}

func TestARequestSurvivesAnAgentRestart(t *testing.T) {
	agent := &fakeAgent{refusals: 1}
	s := New(agent)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.deliver(ctx)

	_, err := s.Ready(ctx, &sdk.Empty{})
	require.NoError(t, err, "the call is acknowledged even though the agent is away")

	assert.Eventually(t, func() bool { return len(agent.events()) == 1 }, 3*retryDelay, 10*time.Millisecond)
}

func TestReserveAsksForReadyWhenTimeIsUp(t *testing.T) {
	agent := &fakeAgent{}
	s := New(agent)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.deliver(ctx)

	_, err := s.Reserve(ctx, &sdk.Duration{Seconds: 1})
	require.NoError(t, err)

	want := []sidecarv1.Event{sidecarv1.Event_EVENT_RESERVE, sidecarv1.Event_EVENT_READY}
	assert.Eventually(t, func() bool { return assert.ObjectsAreEqual(want, agent.events()) }, 3*time.Second, 20*time.Millisecond)
	assert.Equal(t, int64(1), agent.told[0].GetReserveSeconds())
}

func TestSetLabelValidatesLikeUpstream(t *testing.T) {
	agent := &fakeAgent{}
	s := New(agent)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.deliver(ctx)

	_, err := s.SetLabel(ctx, &sdk.KeyValue{Key: "not a valid key!", Value: "x"})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	_, err = s.SetLabel(ctx, &sdk.KeyValue{Key: "mode", Value: "not a valid value!"})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	_, err = s.SetAnnotation(ctx, &sdk.KeyValue{Key: "note", Value: "annotations may hold anything!"})
	assert.NoError(t, err)

	_, err = s.SetLabel(ctx, &sdk.KeyValue{Key: "mode", Value: "ranked"})
	require.NoError(t, err)
	assert.Eventually(t, func() bool { return len(agent.events()) == 2 }, 2*time.Second, 10*time.Millisecond)
	assert.Equal(t, map[string]string{"mode": "ranked"}, agent.told[1].GetLabels())
}

func TestGetWaitsForTheFirstGameServer(t *testing.T) {
	s := New(&fakeAgent{})

	short, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := s.GetGameServer(short, &sdk.Empty{})
	assert.ErrorIs(t, err, context.DeadlineExceeded, "nothing has arrived yet")

	s.set(&sdk.GameServer{Status: &sdk.GameServer_Status{State: "Ready"}})
	gs, err := s.GetGameServer(context.Background(), &sdk.Empty{})
	require.NoError(t, err)
	assert.Equal(t, "Ready", gs.GetStatus().GetState())
}

func TestWatchersSeeTheCurrentStateThenTheLatest(t *testing.T) {
	s := New(&fakeAgent{})
	s.set(&sdk.GameServer{Status: &sdk.GameServer_Status{State: "Scheduled"}})

	updates, cancel := s.subscribe()
	defer cancel()
	assert.Equal(t, "Scheduled", (<-updates).GetStatus().GetState(), "a new watcher starts with the current state")

	// A slow watcher skips straight to the newest state.
	s.set(&sdk.GameServer{Status: &sdk.GameServer_Status{State: "RequestReady"}})
	s.set(&sdk.GameServer{Status: &sdk.GameServer_Status{State: "Ready"}})
	assert.Equal(t, "Ready", (<-updates).GetStatus().GetState())
}

func TestHealthCountsMissedPeriods(t *testing.T) {
	start := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	period := 5 * time.Second
	h := &health{}
	h.touch(start)

	assert.Equal(t, int32(0), h.check(start.Add(4*time.Second), period), "a ping within the period is fine")
	assert.Equal(t, int32(1), h.check(start.Add(6*time.Second), period))
	assert.Equal(t, int32(2), h.check(start.Add(11*time.Second), period))

	h.touch(start.Add(12 * time.Second))
	assert.Equal(t, int32(0), h.check(start.Add(13*time.Second), period), "any ping starts the count again")
}

func TestAlphaAnswersLikeUpstreamWithTheGateOff(t *testing.T) {
	_, err := alpha{}.GetPlayerCount(context.Background(), nil)
	assert.EqualError(t, err, "PlayerTracking not enabled")
	assert.Equal(t, codes.Unknown, status.Code(err))
}
