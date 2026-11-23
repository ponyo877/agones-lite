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
)

// controlPlane is a store with the node API served over TLS on a local port.
type controlPlane struct {
	store   *store.Store
	local   *Local
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
	nodev1.RegisterNodesServer(server, NewServer(s, local))
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	return controlPlane{store: s, local: local, secrets: secrets, address: listener.Addr().String()}
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
