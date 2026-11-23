package conformance

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	sdkpb "agones.dev/agones/pkg/sdk"
	sdk "agones.dev/agones/sdks/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// connect is what a game server does first: sdk.NewSDK finds the SDK
// server through the environment the agent set for the container.
func connect(t *testing.T, grpcPort int) *sdk.SDK {
	t.Helper()
	t.Setenv("AGONES_SDK_GRPC_PORT", strconv.Itoa(grpcPort))
	game, err := sdk.NewSDK()
	require.NoError(t, err)
	return game
}

// beat reports health four times a second until the test ends or stop is called.
func beat(t *testing.T, game *sdk.SDK) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		for ctx.Err() == nil {
			_ = game.Health()
			time.Sleep(250 * time.Millisecond)
		}
	}()
	return cancel
}

// states collects the states a game server is shown by WatchGameServer.
type states struct {
	mu   sync.Mutex
	seen []string
}

func (s *states) add(gs *sdkpb.GameServer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := gs.GetStatus().GetState()
	if len(s.seen) == 0 || s.seen[len(s.seen)-1] != state {
		s.seen = append(s.seen, state)
	}
}

// lasting leaves out the states a game server only passes through. A
// watcher is shown the latest state, so it may or may not catch those.
func (s *states) lasting() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, state := range s.seen {
		passing := state == "Scheduled" || state == "RequestReady"
		if !passing && (len(out) == 0 || out[len(out)-1] != state) {
			out = append(out, state)
		}
	}
	return out
}

func TestAGameServerLivesItsWholeLifeThroughTheOfficialSDK(t *testing.T) {
	b := newBench(t)
	grpcPort, _ := b.create("official")
	game := connect(t, grpcPort)
	beat(t, game)
	watched := &states{}
	require.NoError(t, game.WatchGameServer(watched.add))

	// Ready, and what the game server learns about itself.
	require.NoError(t, game.Ready())
	b.becomes("official", agonesv1.GameServerStateReady)
	var self *sdkpb.GameServer
	b.soon(func() bool {
		var err error
		self, err = game.GameServer()
		return err == nil && self.GetStatus().GetState() == "Ready"
	}, "the game server sees itself Ready")
	assert.Equal(t, "official", self.GetObjectMeta().GetName())
	assert.Equal(t, "default", self.GetObjectMeta().GetNamespace())
	assert.Equal(t, "203.0.113.10", self.GetStatus().GetAddress())
	require.Len(t, self.GetStatus().GetPorts(), 1)
	assert.Equal(t, b.stored("official").Status.Ports[0].Port, self.GetStatus().GetPorts()[0].GetPort())
	assert.Equal(t, int32(1), self.GetSpec().GetHealth().GetPeriodSeconds())

	// Labels and annotations get the SDK's prefix.
	require.NoError(t, game.SetLabel("mode", "ranked"))
	require.NoError(t, game.SetAnnotation("map", "dust"))
	b.soon(func() bool {
		gs := b.stored("official")
		return gs.Labels["agones.dev/sdk-mode"] == "ranked" && gs.Annotations["agones.dev/sdk-map"] == "dust"
	}, "the label and the annotation arrive")

	// Reserved for a second, then Ready again by itself.
	require.NoError(t, game.Reserve(time.Second))
	b.becomes("official", agonesv1.GameServerStateReserved)
	b.becomes("official", agonesv1.GameServerStateReady)

	// A game server may allocate itself.
	require.NoError(t, game.Allocate())
	b.becomes("official", agonesv1.GameServerStateAllocated)

	// Player tracking was alpha and off by default upstream; it is off here.
	_, err := game.Alpha().GetPlayerCapacity()
	assert.ErrorContains(t, err, "PlayerTracking not enabled")

	// WatchGameServer showed every state that lasted.
	b.soon(func() bool {
		return assert.ObjectsAreEqual([]string{"Ready", "Reserved", "Ready", "Allocated"}, watched.lasting())
	}, "the watcher sees the states in order")

	// The end: Shutdown takes the GameServer away.
	require.NoError(t, game.Shutdown())
	b.soon(func() bool { return b.stored("official") == nil }, "the GameServer is gone")
}

func TestCountersAndListsThroughTheOfficialSDK(t *testing.T) {
	b := newBench(t)
	grpcPort, _ := b.create("counting")
	game := connect(t, grpcPort)
	beat(t, game)
	require.NoError(t, game.Ready())
	b.becomes("counting", agonesv1.GameServerStateReady)
	counts := game.Beta()

	// A Counter: one of ten rooms is in use.
	count, err := counts.GetCounterCount("rooms")
	require.NoError(t, err)
	assert.Equal(t, int64(1), count)
	capacity, err := counts.GetCounterCapacity("rooms")
	require.NoError(t, err)
	assert.Equal(t, int64(10), capacity)

	require.NoError(t, counts.IncrementCounter("rooms", 4))
	require.NoError(t, counts.DecrementCounter("rooms", 2))
	count, err = counts.GetCounterCount("rooms")
	require.NoError(t, err)
	assert.Equal(t, int64(3), count, "a read right after a write shows the write")

	require.Error(t, counts.IncrementCounter("rooms", 8), "eleven rooms do not fit into ten")
	require.NoError(t, counts.SetCounterCapacity("rooms", 20))
	require.NoError(t, counts.SetCounterCount("rooms", 11))
	_, err = counts.GetCounterCount("lobbies")
	require.Error(t, err, "a Counter must be declared in the GameServer")

	// A List: the players who are connected.
	values, err := counts.GetListValues("players")
	require.NoError(t, err)
	assert.Equal(t, []string{"ada"}, values)
	require.NoError(t, counts.AppendListValue("players", "bob"))
	require.Error(t, counts.AppendListValue("players", "bob"), "a value is in a List once")
	require.NoError(t, counts.DeleteListValue("players", "ada"))
	require.Error(t, counts.DeleteListValue("players", "ada"), "it is gone already")
	has, err := counts.ListContains("players", "bob")
	require.NoError(t, err)
	assert.True(t, has)
	length, err := counts.GetListLength("players")
	require.NoError(t, err)
	assert.Equal(t, 1, length)
	require.NoError(t, counts.SetListCapacity("players", 2))
	capacity, err = counts.GetListCapacity("players")
	require.NoError(t, err)
	assert.Equal(t, int64(2), capacity)
	require.NoError(t, counts.AppendListValue("players", "cy"))
	require.Error(t, counts.AppendListValue("players", "di"), "the List is full")

	// All of it ends up in the GameServer, where allocation and
	// autoscaling read it.
	b.soon(func() bool {
		status := b.stored("counting").Status
		rooms, players := status.Counters["rooms"], status.Lists["players"]
		return rooms.Count == 11 && rooms.Capacity == 20 &&
			players.Capacity == 2 && assert.ObjectsAreEqual([]string{"bob", "cy"}, players.Values)
	}, "the control plane has the same numbers")
}

func TestAGameServerThatGoesQuietBecomesUnhealthy(t *testing.T) {
	b := newBenchOverTCP(t)
	grpcPort, _ := b.create("quiet")
	game := connect(t, grpcPort)
	stop := beat(t, game)
	require.NoError(t, game.Ready())
	b.becomes("quiet", agonesv1.GameServerStateReady)

	// While it reports, it stays Ready well past the two seconds it is given.
	time.Sleep(3 * time.Second)
	require.Equal(t, agonesv1.GameServerStateReady, b.stored("quiet").Status.State)

	stop()
	b.becomes("quiet", agonesv1.GameServerStateUnhealthy)
}
