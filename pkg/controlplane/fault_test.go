package controlplane

import (
	"context"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ponyo877/scrim/pkg/agent"
	"github.com/ponyo877/scrim/pkg/agones"
	scrimv1 "github.com/ponyo877/scrim/pkg/apis/scrim/v1"
	"github.com/ponyo877/scrim/pkg/auth"
	"github.com/ponyo877/scrim/pkg/nodes"
	"github.com/ponyo877/scrim/pkg/store"
	nodev1 "github.com/ponyo877/scrim/proto/node/v1"
)

// These tests break things on purpose. They need a control plane that can
// be restarted on the state it left behind, and nodes that can go quiet, so
// the nodes here dial in over the network as they do from other machines.

// impatient gives up idle game servers quickly, so the tests need not wait
// five minutes. It still leaves an agent ample time to dial again.
var impatient = nodes.Graces{Unallocated: 6 * time.Second, Allocated: time.Hour}

// site is a control plane together with the directory it keeps its state
// in and the address its agents dial, both of which outlive a restart.
type site struct {
	world
	dir     string
	address string
	token   string
	halt    func(crash bool)
	agents  map[string]func()
}

func newSite(t *testing.T) *site {
	t.Helper()
	require.NoError(t, agones.Setup())
	s := &site{world: world{t: t}, dir: t.TempDir(), address: "127.0.0.1:0", agents: map[string]func(){}}
	s.start()
	t.Cleanup(func() {
		for _, leave := range s.agents {
			leave()
		}
		s.halt(false)
	})
	return s
}

// start runs a control plane on whatever state the directory holds.
func (s *site) start() {
	s.t.Helper()
	state, err := store.Open(filepath.Join(s.dir, "state.db"), store.Kinds())
	require.NoError(s.t, err)
	secrets, err := auth.Load(s.dir, nil)
	require.NoError(s.t, err)
	cp, err := New(state, nil, nil, impatient)
	require.NoError(s.t, err)

	listener, err := net.Listen("tcp", s.address)
	require.NoError(s.t, err)
	options := append(auth.RequireGRPC(secrets, auth.Node), grpc.Creds(credentials.NewTLS(secrets.ServerTLS())))
	server := grpc.NewServer(options...)
	nodev1.RegisterNodesServer(server, nodes.NewServer(state, cp.Agents))
	ctx, cancel := context.WithCancel(context.Background())
	var running sync.WaitGroup
	running.Go(func() { state.Run(ctx) })
	running.Go(func() { cp.Run(ctx) })
	go func() { _ = server.Serve(listener) }()

	s.world = world{t: s.t, cp: cp, api: cp.API.Handler(), store: state}
	s.address, s.token = listener.Addr().String(), secrets.Token(auth.Node)
	s.halt = func(crash bool) {
		if crash {
			// Nothing gets written on the way down: the file stays as it
			// was at the moment of the crash.
			_ = state.Abandon()
		}
		server.Stop()
		cancel()
		running.Wait()
		_ = state.Close()
	}
}

// join starts a simulated agent that dials in like a real one.
func (s *site) join(name string) {
	s.t.Helper()
	remote, err := nodes.Dial(s.address, s.token)
	require.NoError(s.t, err)
	node := &scrimv1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: scrimv1.NodeStatus{
			Phase:     scrimv1.NodeReady,
			Address:   "203.0.113.10",
			PortRange: scrimv1.PortRange{Min: 7000, Max: 7100},
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	go remote.Run(ctx, name)
	go func() { _ = agent.NewSimulator(node, remote).Run(ctx) }()
	s.agents[name] = func() {
		cancel()
		_ = remote.Close()
	}
}

// leave makes a node go quiet, as when its machine dies.
func (s *site) leave(name string) {
	s.agents[name]()
	delete(s.agents, name)
}

func (s *site) phase(node string) scrimv1.NodePhase {
	n, _ := store.NodesOf(s.store).Get("", node)
	if n == nil {
		return ""
	}
	return n.Status.Phase
}

// on lists the game servers on a node that have not ended, by name.
func (s *site) on(node string) map[string]agonesv1.GameServerState {
	servers := map[string]agonesv1.GameServerState{}
	for _, gs := range store.GameServersOf(s.store).List("default") {
		if gs.Status.NodeName == node && gs.DeletionTimestamp == nil {
			servers[gs.Name] = gs.Status.State
		}
	}
	return servers
}

// spread applies a Fleet of four that takes two game servers on each of
// the two nodes, and hands one of them to a matchmaker. It returns the
// game server with the session and the node it is on.
func (s *site) spread() (session, node string) {
	s.t.Helper()
	s.join("node-1")
	s.join("node-2")
	s.soon(func() bool { return s.phase("node-1") == scrimv1.NodeReady && s.phase("node-2") == scrimv1.NodeReady }, "both nodes join")
	manifest := strings.Replace(fleetManifest, "replicas: 3", "replicas: 4\n  scheduling: Distributed", 1)
	s.call(http.MethodPut, "/v1/namespaces/default/fleets/simple", manifest)
	s.soon(func() bool {
		return s.fleet().Status.ReadyReplicas == 4 && len(s.on("node-1")) == 2 && len(s.on("node-2")) == 2
	}, "four game servers become Ready, two on each node")
	got, err := s.allocate()
	require.NoError(s.t, err)
	return got.GetGameServerName(), got.GetNodeName()
}

func other(node string) string {
	if node == "node-1" {
		return "node-2"
	}
	return "node-1"
}

func TestALostNodeIsReplacedAndItsSessionIsKept(t *testing.T) {
	s := newSite(t)
	session, lost := s.spread()
	kept := other(lost)

	// The machine with the session on it drops off the network.
	s.leave(lost)
	s.soon(func() bool { return s.phase(lost) == scrimv1.NodeDisconnected }, "the control plane notices at once")

	// Nothing on it is handed out any more, although it still looks Ready.
	for range 2 {
		got, err := s.allocate()
		require.NoError(t, err)
		assert.Equal(t, kept, got.GetNodeName())
	}

	// Its idle game server is given up and replaced where there is room.
	// The session may be going on unharmed, so it is waited for.
	s.soon(func() bool {
		status := s.fleet().Status
		return status.AllocatedReplicas == 3 && status.ReadyReplicas == 1 && len(s.on(lost)) == 1
	}, "the idle game server is replaced, the session stays")
	assert.Equal(t, map[string]agonesv1.GameServerState{session: agonesv1.GameServerStateAllocated}, s.on(lost))

	// The machine comes back and is used again.
	s.join(lost)
	s.soon(func() bool { return s.phase(lost) == scrimv1.NodeReady }, "the node is back")
	s.call(http.MethodPut, "/v1/namespaces/default/fleets/simple/scale", `{"replicas": 8}`)
	s.soon(func() bool { return s.fleet().Status.ReadyReplicas == 5 && len(s.on(lost)) > 1 }, "new game servers land on it")
}

func TestACrashKeepsWhatIsStillThereAndGivesUpWhatIsNot(t *testing.T) {
	s := newSite(t)
	session, kept := s.spread()
	lost := other(kept)
	before := s.on(kept)

	// The control plane crashes, and while it is down a machine dies. The
	// state it left behind still calls both nodes Ready.
	s.halt(true)
	s.leave(lost)
	s.start()

	// It trusts neither: the node that is still there says so, the other
	// one is now counted as out of reach.
	assert.Equal(t, scrimv1.NodeDisconnected, s.phase(lost))
	s.soon(func() bool { return s.phase(kept) == scrimv1.NodeReady }, "the agent that is still there dials in again")
	assert.Equal(t, scrimv1.NodeDisconnected, s.phase(lost))

	// The game servers that were running are the same ones, session included.
	assert.Equal(t, before, s.on(kept))
	assert.Equal(t, agonesv1.GameServerStateAllocated, before[session])

	// Those on the dead machine are replaced once they have been waited for.
	s.soon(func() bool {
		status := s.fleet().Status
		return status.AllocatedReplicas == 1 && status.ReadyReplicas == 3 && len(s.on(lost)) == 0 && len(s.on(kept)) == 4
	}, "the Fleet is whole again on the node that is left")
	for name, state := range before {
		assert.Equal(t, state, s.on(kept)[name], "%s was not disturbed", name)
	}
}

// The store writes to its file behind what it holds in memory, and a
// crash takes back whatever the file did not have yet. An allocation is
// answered when the file has it. So whatever the control plane was in the
// middle of when it crashed, a matchmaker that got an answer finds its
// game server Allocated afterwards, and no two were given the same one.
func TestWhatAMatchmakerWasToldOutlastsACrash(t *testing.T) {
	s := newSite(t)
	s.join("node-1")
	s.call(http.MethodPut, "/v1/namespaces/default/fleets/simple", strings.Replace(fleetManifest, "replicas: 3", "replicas: 60", 1))
	s.soon(func() bool { return s.fleet().Status.ReadyReplicas == 60 }, "sixty game servers become Ready")

	told := make(chan string, 60)
	var asking sync.WaitGroup
	for range 8 {
		asking.Go(func() { s.allocateInto(told) })
	}
	for len(told) < 20 { // in the middle of it
		time.Sleep(time.Millisecond)
	}
	s.halt(true)
	asking.Wait()
	close(told)
	s.start()

	after := s.on("node-1")
	seen := map[string]bool{}
	for name := range told {
		assert.Equal(t, agonesv1.GameServerStateAllocated, after[name], "%s was handed to a matchmaker", name)
		assert.False(t, seen[name], "%s was handed out twice", name)
		seen[name] = true
	}
}

// allocateInto allocates until it is refused, by a crash or because
// nothing is left to hand out, and sends on the name of every game server
// it was given.
func (s *site) allocateInto(told chan<- string) {
	for {
		got, err := s.allocate()
		if err != nil || got.GetGameServerName() == "" {
			return
		}
		told <- got.GetGameServerName()
	}
}
