package nodes

import (
	"context"
	"errors"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ponyo877/agones-lite/pkg/agent"
	"github.com/ponyo877/agones-lite/pkg/agones"
	litev1 "github.com/ponyo877/agones-lite/pkg/apis/lite/v1"
	"github.com/ponyo877/agones-lite/pkg/auth"
	"github.com/ponyo877/agones-lite/pkg/gameservers"
	"github.com/ponyo877/agones-lite/pkg/store"
	nodev1 "github.com/ponyo877/agones-lite/proto/node/v1"
	sidecarv1 "github.com/ponyo877/agones-lite/proto/sidecar/v1"
)

// controlPlane is a store with the node API served over TLS on a local port.
type controlPlane struct {
	store   *store.Store
	local   *Local
	server  *Server
	secrets *auth.Secrets
	address string
}

func newControlPlane(t *testing.T) controlPlane {
	t.Helper()
	require.NoError(t, agones.Setup())
	s, err := store.Open(filepath.Join(t.TempDir(), "state.db"), store.Kinds())
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	secrets, err := auth.Load(t.TempDir(), nil)
	require.NoError(t, err)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	options := append(auth.RequireGRPC(secrets, auth.Node), grpc.Creds(credentials.NewTLS(secrets.ServerTLS())))
	server := grpc.NewServer(options...)
	local := NewLocal(s, gameservers.NewController(s, nil))
	agents := NewServer(s, local)
	nodev1.RegisterNodesServer(server, agents)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	return controlPlane{store: s, local: local, server: agents, secrets: secrets, address: listener.Addr().String()}
}

func node(name string) *litev1.Node {
	return &litev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status:     litev1.NodeStatus{Phase: litev1.NodeReady, Address: "203.0.113.10"},
	}
}

func placed(name, nodeName string) *agonesv1.GameServer {
	gs := &agonesv1.GameServer{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name}}
	gs.Status.State = agonesv1.GameServerStateCreating
	gs.Status.NodeName = nodeName
	return gs
}

func soon(t *testing.T, condition func() bool, message string) {
	t.Helper()
	require.Eventually(t, condition, 5*time.Second, 10*time.Millisecond, message)
}

func TestAnAgentOnAnotherMachineSeesItsGameServers(t *testing.T) {
	cp := newControlPlane(t)
	servers := store.GameServersOf(cp.store)
	_, err := servers.Create(placed("before", "node-1"))
	require.NoError(t, err)
	_, err = servers.Create(placed("elsewhere", "node-2"))
	require.NoError(t, err)

	remote, err := Dial(cp.address, cp.secrets.Token(auth.Node))
	require.NoError(t, err)
	t.Cleanup(func() { _ = remote.Close() })
	_, current := remote.Assigned("node-1")
	assert.False(t, current, "nothing is known before the first answer")

	require.NoError(t, remote.Register(node("node-1")))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go remote.Run(ctx, "node-1")

	names := func() []string {
		list, current := remote.Assigned("node-1")
		if !current {
			return nil
		}
		out := []string{}
		for _, gs := range list {
			out = append(out, gs.Name)
		}
		return out
	}
	soon(t, func() bool { return assert.ObjectsAreEqual([]string{"before"}, names()) }, "the existing GameServer arrives")

	_, err = servers.Create(placed("after", "node-1"))
	require.NoError(t, err)
	soon(t, func() bool { return len(names()) == 2 }, "a new GameServer arrives")

	require.NoError(t, servers.Delete("default", "before"))
	soon(t, func() bool { return assert.ObjectsAreEqual([]string{"after"}, names()) }, "a removed GameServer goes away")
}

func TestReportsFromAnAgentChangeTheGameServer(t *testing.T) {
	cp := newControlPlane(t)
	servers := store.GameServersOf(cp.store)
	_, err := servers.Create(placed("gs", "node-1"))
	require.NoError(t, err)
	remote, err := Dial(cp.address, cp.secrets.Token(auth.Node))
	require.NoError(t, err)
	t.Cleanup(func() { _ = remote.Close() })

	require.NoError(t, remote.Report(gameservers.Report{Namespace: "default", Name: "gs", Event: gameservers.ContainersCreated}))
	gs, _ := servers.Get("default", "gs")
	assert.Equal(t, agonesv1.GameServerStateStarting, gs.Status.State)

	require.NoError(t, remote.SetMetadata("default", "gs", map[string]string{"mode": "ranked"}, nil))
	gs, _ = servers.Get("default", "gs")
	assert.Equal(t, "ranked", gs.Labels["agones.dev/sdk-mode"])

	err = remote.Report(gameservers.Report{Namespace: "default", Name: "gs", Event: gameservers.Readied})
	assert.Equal(t, codes.InvalidArgument, status.Code(err), "granting Ready is the control plane's decision")
}

func TestAReserveTooLongForADurationLastsAsLongAsOneCan(t *testing.T) {
	cp := newControlPlane(t)
	servers := store.GameServersOf(cp.store)
	_, err := servers.Create(inState("gs", agonesv1.GameServerStateReady))
	require.NoError(t, err)

	// So many seconds that, as nanoseconds, they wrap around to 0.3s.
	_, err = cp.server.Report(context.Background(), &nodev1.ReportRequest{
		Namespace: "default", Name: "gs", Event: string(gameservers.SDKReserve), ReserveSeconds: 18_446_744_074,
	})

	require.NoError(t, err)
	gs, _ := servers.Get("default", "gs")
	assert.Equal(t, agonesv1.GameServerStateReserved, gs.Status.State)
	require.NotNil(t, gs.Status.ReservedUntil)
	assert.Greater(t, time.Until(gs.Status.ReservedUntil.Time), 200*365*24*time.Hour)
}

func TestTheNodeIsDisconnectedOnceItsAgentIsGone(t *testing.T) {
	cp := newControlPlane(t)
	nodes := store.NodesOf(cp.store)
	remote, err := Dial(cp.address, cp.secrets.Token(auth.Node))
	require.NoError(t, err)
	t.Cleanup(func() { _ = remote.Close() })
	require.NoError(t, remote.Register(node("node-1")))

	ctx, cancel := context.WithCancel(context.Background())
	go remote.Run(ctx, "node-1")
	soon(t, func() bool { _, current := remote.Assigned("node-1"); return current }, "the agent connects")
	n, _ := nodes.Get("", "node-1")
	assert.Equal(t, litev1.NodeReady, n.Status.Phase)

	cancel()
	soon(t, func() bool {
		n, _ := nodes.Get("", "node-1")
		return n.Status.Phase == litev1.NodeDisconnected
	}, "the control plane notices the agent is gone")
}

func TestARemoteNodeIsOnlyReadyWhileItWatches(t *testing.T) {
	cp := newControlPlane(t)
	remote, err := Dial(cp.address, cp.secrets.Token(auth.Node))
	require.NoError(t, err)
	t.Cleanup(func() { _ = remote.Close() })
	phase := func() litev1.NodePhase {
		n, _ := store.NodesOf(cp.store).Get("", "node-1")
		return n.Status.Phase
	}

	// An agent that registers and dies before it watches leaves no call
	// whose end would tell us so.
	require.NoError(t, remote.Register(node("node-1")))
	assert.Equal(t, litev1.NodeDisconnected, phase())

	ctx, cancel := context.WithCancel(context.Background())
	go remote.Run(ctx, "node-1")
	soon(t, func() bool { return phase() == litev1.NodeReady }, "watching is what connects the node")
	require.NoError(t, remote.Register(node("node-1")))
	assert.Equal(t, litev1.NodeReady, phase(), "a heartbeat changes nothing about that")

	cancel()
	soon(t, func() bool { return phase() == litev1.NodeDisconnected }, "the control plane notices the agent is gone")
	// The agent's last heartbeat was still on its way.
	require.NoError(t, remote.Register(node("node-1")))
	assert.Equal(t, litev1.NodeDisconnected, phase(), "or the node would be Ready for ever")
}

func TestARemoteNodeThatWatchesBeforeItRegistersIsReady(t *testing.T) {
	cp := newControlPlane(t)
	remote, err := Dial(cp.address, cp.secrets.Token(auth.Node))
	require.NoError(t, err)
	t.Cleanup(func() { _ = remote.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go remote.Run(ctx, "node-1")
	soon(t, func() bool { _, current := remote.Assigned("node-1"); return current }, "the agent connects")

	// A new agent does both at once, and this may well be the order.
	require.NoError(t, remote.Register(node("node-1")))

	n, _ := store.NodesOf(cp.store).Get("", "node-1")
	assert.Equal(t, litev1.NodeReady, n.Status.Phase)
}

func TestAnAgentInTheSameProcessIsReadyByRegistering(t *testing.T) {
	cp := newControlPlane(t)
	nodes := store.NodesOf(cp.store)
	n := node("node-1")
	n.Status.Phase = litev1.NodeDisconnected // as StartOver leaves every node
	_, err := nodes.Create(n)
	require.NoError(t, err)

	require.NoError(t, cp.local.Register(node("node-1")))

	n, _ = nodes.Get("", "node-1")
	assert.Equal(t, litev1.NodeReady, n.Status.Phase, "it has no Watch call to be judged by")
}

// stalledStream is the control plane's end of a Watch call whose agent has
// stopped reading: nothing can be sent until the agent reads again.
type stalledStream struct {
	grpc.ServerStream
	ctx     context.Context
	stalled chan struct{} // closed once something waits to be sent
	resumed chan struct{} // closed when the agent reads again
}

func (s *stalledStream) Context() context.Context { return s.ctx }

func (s *stalledStream) Send(*nodev1.WatchResponse) error {
	select {
	case <-s.stalled:
	default:
		close(s.stalled)
	}
	<-s.resumed
	return nil
}

func TestAnAgentThatFallsBehindIsCutOffSoThatItStartsAgain(t *testing.T) {
	cp := newControlPlane(t)
	servers := store.GameServersOf(cp.store)
	gs, err := servers.Create(placed("gs", "node-1"))
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := &stalledStream{ctx: ctx, stalled: make(chan struct{}), resumed: make(chan struct{})}
	ended := make(chan error, 1)
	go func() {
		ended <- cp.server.Watch(&nodev1.WatchRequest{Node: "node-1"}, stream)
	}()
	<-stream.stalled

	// One change more than the store keeps for a watcher that does not read.
	unread, stop := servers.Watch()
	defer stop()
	for range cap(unread) + 1 {
		gs, err = servers.Update(gs)
		require.NoError(t, err)
	}
	close(stream.resumed)

	select {
	case err := <-ended:
		assert.Equal(t, codes.Aborted, status.Code(err), "an error makes the agent dial again and receive everything anew")
	case <-time.After(5 * time.Second):
		assert.Fail(t, "the call goes on, and the agent's copy keeps a hole it cannot know of")
	}
}

func TestAWrongJoinTokenIsRefused(t *testing.T) {
	cp := newControlPlane(t)
	fingerprint := cp.secrets.Token(auth.Node)[:64]

	wrongSecret, err := Dial(cp.address, fingerprint+".not-the-secret")
	require.NoError(t, err)
	t.Cleanup(func() { _ = wrongSecret.Close() })
	assert.Equal(t, codes.Unauthenticated, status.Code(wrongSecret.Register(node("node-1"))))

	other, err := auth.Load(t.TempDir(), nil)
	require.NoError(t, err)
	wrongServer, err := Dial(cp.address, other.Token(auth.Node))
	require.NoError(t, err)
	t.Cleanup(func() { _ = wrongServer.Close() })
	assert.Error(t, wrongServer.Register(node("node-1")), "the agent must not talk to a control plane with another CA")

	_, err = Dial(cp.address, "no-dot-in-it")
	assert.Error(t, err)
}

// fixedLog pretends to be an agent whose every container wrote the same thing.
type fixedLog string

func (f fixedLog) Logs(_ context.Context, req agent.LogRequest) (io.ReadCloser, error) {
	if req.Name == "broken" {
		return nil, errors.New("no such container")
	}
	return io.NopCloser(strings.NewReader(string(f) + " for " + req.Name)), nil
}

func TestLogsTravelFromARemoteAgentToTheControlPlane(t *testing.T) {
	cp := newControlPlane(t)
	servers := store.GameServersOf(cp.store)
	for _, name := range []string{"gs", "broken"} {
		_, err := servers.Create(placed(name, "node-1"))
		require.NoError(t, err)
	}
	_, err := servers.Create(placed("far", "node-9"))
	require.NoError(t, err)

	remote, err := Dial(cp.address, cp.secrets.Token(auth.Node))
	require.NoError(t, err)
	t.Cleanup(func() { _ = remote.Close() })
	remote.Serve(fixedLog("the log"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go remote.Run(ctx, "node-1")
	soon(t, func() bool { _, current := remote.Assigned("node-1"); return current }, "the agent connects")

	read := func(name string) (string, error) {
		stream, err := cp.local.Logs(ctx, agent.LogRequest{Namespace: "default", Name: name})
		if err != nil {
			return "", err
		}
		defer func() { _ = stream.Close() }()
		text, err := io.ReadAll(stream)
		return string(text), err
	}

	text, err := read("gs")
	require.NoError(t, err)
	assert.Equal(t, "the log for gs", text)

	_, err = read("broken")
	assert.ErrorContains(t, err, "no such container", "the agent's error reaches the reader")

	_, err = read("far")
	assert.ErrorContains(t, err, "not connected")

	_, err = read("nobody")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestLogsSurviveAnAgentThatDialsAgainBeforeItsOldCallEnds(t *testing.T) {
	cp := newControlPlane(t)
	_, err := store.GameServersOf(cp.store).Create(placed("gs", "node-1"))
	require.NoError(t, err)
	watching := func(ctx context.Context, log string) {
		remote, err := Dial(cp.address, cp.secrets.Token(auth.Node))
		require.NoError(t, err)
		t.Cleanup(func() { _ = remote.Close() })
		remote.Serve(fixedLog(log))
		go remote.Run(ctx, "node-1")
		soon(t, func() bool { _, current := remote.Assigned("node-1"); return current }, "the agent connects")
	}
	read := func() string {
		stream, err := cp.local.Logs(context.Background(), agent.LogRequest{Namespace: "default", Name: "gs"})
		if err != nil {
			return err.Error()
		}
		defer func() { _ = stream.Close() }()
		text, _ := io.ReadAll(stream)
		return string(text)
	}
	old, endOld := context.WithCancel(context.Background())
	watching(old, "old")
	current, endCurrent := context.WithCancel(context.Background())
	defer endCurrent()
	watching(current, "new") // the control plane has not noticed yet that the old call is dead
	require.Equal(t, "new for gs", read())

	endOld()

	soon(t, func() bool {
		cp.server.mu.Lock()
		defer cp.server.mu.Unlock()
		return cp.server.streams["node-1"] == 1
	}, "the old call ends")
	assert.Equal(t, "new for gs", read(), "the call that ended must not take the other one's reader with it")
}

func TestCountsChangedThroughTheSDKReachTheGameServer(t *testing.T) {
	cp := newControlPlane(t)
	servers := store.GameServersOf(cp.store)
	gs := placed("gs", "node-1")
	gs.Status.Counters = map[string]agonesv1.CounterStatus{"rooms": {Count: 1, Capacity: 10}}
	gs.Status.Lists = map[string]agonesv1.ListStatus{"players": {Capacity: 4, Values: []string{"ada"}}}
	_, err := servers.Create(gs)
	require.NoError(t, err)
	remote, err := Dial(cp.address, cp.secrets.Token(auth.Node))
	require.NoError(t, err)
	t.Cleanup(func() { _ = remote.Close() })

	change := func(id string) (*agonesv1.GameServer, error) {
		return remote.UpdateCounts("default", "gs", id,
			[]*sidecarv1.CounterUpdate{{Name: "rooms", CountDiff: 2}},
			[]*sidecarv1.ListUpdate{{Name: "players", Add: []string{"bob"}, Remove: []string{"ada"}}})
	}
	after, err := change("first")

	require.NoError(t, err)
	assert.Equal(t, int64(3), after.Status.Counters["rooms"].Count)
	assert.Equal(t, []string{"bob"}, after.Status.Lists["players"].Values)
	stored, _ := servers.Get("default", "gs")
	assert.Equal(t, after.Status, stored.Status)
	assert.Equal(t, after.ResourceVersion, stored.ResourceVersion, "the answer is the GameServer as it was stored")

	// The same change again, as after a lost answer, is not made twice.
	again, err := change("first")
	require.NoError(t, err)
	assert.Equal(t, int64(3), again.Status.Counters["rooms"].Count)
	assert.Equal(t, stored.ResourceVersion, again.ResourceVersion)

	// Another change that asks for the same thing is.
	next, err := change("second")
	require.NoError(t, err)
	assert.Equal(t, int64(5), next.Status.Counters["rooms"].Count)

	// A GameServer that is gone has nothing to answer with.
	gone, err := remote.UpdateCounts("default", "nobody", "third", []*sidecarv1.CounterUpdate{{Name: "rooms", CountDiff: 1}}, nil)
	require.NoError(t, err)
	assert.Nil(t, gone)
}
