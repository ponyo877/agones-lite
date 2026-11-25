package nodes

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ponyo877/agones-lite/pkg/agones"
	litev1 "github.com/ponyo877/agones-lite/pkg/apis/lite/v1"
	"github.com/ponyo877/agones-lite/pkg/gameservers"
	"github.com/ponyo877/agones-lite/pkg/store"
)

var noon = time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)

func inState(name string, state agonesv1.GameServerState) *agonesv1.GameServer {
	gs := placed(name, "node-1")
	gs.Status.State = state
	return gs
}

func disconnectedFor(d time.Duration) *litev1.Node {
	n := node("node-1")
	n.Status.Phase = litev1.NodeDisconnected
	n.Status.LastHeartbeatTime = metav1.NewTime(noon.Add(-d))
	return n
}

func names(servers []*agonesv1.GameServer) []string {
	var out []string
	for _, gs := range servers {
		out = append(out, gs.Name)
	}
	return out
}

func TestGivenUp(t *testing.T) {
	onNode := []*agonesv1.GameServer{
		inState("ready", agonesv1.GameServerStateReady),
		inState("reserved", agonesv1.GameServerStateReserved),
		inState("allocated", agonesv1.GameServerStateAllocated),
		inState("already-unhealthy", agonesv1.GameServerStateUnhealthy),
	}
	tests := map[string]struct {
		node *litev1.Node
		want []string
	}{
		"a connected node loses nothing":                 {node: node("node-1")},
		"a short disconnect loses nothing":               {node: disconnectedFor(4 * time.Minute)},
		"after the grace, servers without a session go":  {node: disconnectedFor(5 * time.Minute), want: []string{"ready", "reserved"}},
		"an allocated server is waited for much longer":  {node: disconnectedFor(23 * time.Hour), want: []string{"ready", "reserved"}},
		"after a day, the allocated server goes as well": {node: disconnectedFor(24 * time.Hour), want: []string{"ready", "reserved", "allocated"}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, names(GivenUp(tc.node, onNode, DefaultGraces, noon)))
		})
	}
}

func TestDrainedLeavesSessionsAlone(t *testing.T) {
	onNode := []*agonesv1.GameServer{
		inState("starting", agonesv1.GameServerStateStarting),
		inState("ready", agonesv1.GameServerStateReady),
		inState("reserved", agonesv1.GameServerStateReserved),
		inState("allocated", agonesv1.GameServerStateAllocated),
		inState("shutdown", agonesv1.GameServerStateShutdown),
	}
	draining := node("node-1")
	draining.Spec.Unschedulable, draining.Spec.Drain = true, true
	cordoned := node("node-1")
	cordoned.Spec.Unschedulable = true

	assert.Equal(t, []string{"starting", "ready"}, names(Drained(draining, onNode)))
	assert.Empty(t, Drained(cordoned, onNode), "a cordon alone removes nothing")
}

// controllerFixture is a node Controller on an empty store, driven by hand.
type controllerFixture struct {
	t *testing.T
	*Controller
}

func newControllerFixture(t *testing.T) controllerFixture {
	t.Helper()
	require.NoError(t, agones.Setup())
	s, err := store.Open(filepath.Join(t.TempDir(), "state.db"), store.Kinds())
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	c := NewController(s, gameservers.NewController(s, nil), nil, DefaultGraces)
	c.now = func() time.Time { return noon }
	return controllerFixture{t: t, Controller: c}
}

func (f controllerFixture) add(servers ...*agonesv1.GameServer) {
	f.t.Helper()
	for _, gs := range servers {
		_, err := f.servers.Create(gs)
		require.NoError(f.t, err)
	}
}

// others adds n more nodes, with agents that are connected: a node that is
// gone is then not most of the nodes there are (see MostAreGone).
func (f controllerFixture) others(n int) {
	f.t.Helper()
	for i := range n {
		_, err := f.nodes.Create(node(fmt.Sprint("other-", i)))
		require.NoError(f.t, err)
	}
}

// othersAreBack is what follows a start within seconds: the agents that are
// there dial again.
func (f controllerFixture) othersAreBack() {
	f.t.Helper()
	for _, n := range f.nodes.List("") {
		if strings.HasPrefix(n.Name, "other-") {
			require.NoError(f.t, f.nodes.Change("", n.Name, func(n *litev1.Node) bool {
				n.Status.Phase = litev1.NodeReady
				return true
			}))
		}
	}
}

func (f controllerFixture) sync() {
	f.t.Helper()
	require.NoError(f.t, f.Controller.sync(context.Background(), store.Key{Kind: store.Nodes, Name: "node-1"}))
}

func (f controllerFixture) state(name string) agonesv1.GameServerState {
	f.t.Helper()
	gs, ok := f.servers.Get("default", name)
	require.True(f.t, ok)
	return gs.Status.State
}

func TestALongDisconnectMakesIdleServersUnhealthy(t *testing.T) {
	f := newControllerFixture(t)
	f.others(1)
	_, err := f.nodes.Create(disconnectedFor(10 * time.Minute))
	require.NoError(t, err)
	f.add(inState("ready", agonesv1.GameServerStateReady), inState("allocated", agonesv1.GameServerStateAllocated))

	f.sync()

	assert.Equal(t, agonesv1.GameServerStateUnhealthy, f.state("ready"))
	assert.Equal(t, agonesv1.GameServerStateAllocated, f.state("allocated"), "the session is given a day")
}

// A control plane that was down for longer than the grace finds every node
// that was Disconnected before long overdue, although their agents may all
// be there and are only still dialling.
func TestNothingIsGivenUpRightAfterTheControlPlaneStarted(t *testing.T) {
	f := newControllerFixture(t)
	f.others(1)
	_, err := f.nodes.Create(disconnectedFor(time.Hour))
	require.NoError(t, err)
	f.add(inState("ready", agonesv1.GameServerStateReady), inState("allocated", agonesv1.GameServerStateAllocated))
	_, err = f.servers.Create(placed("orphan", "deleted-node"))
	require.NoError(t, err)
	require.NoError(t, f.StartOver())
	f.othersAreBack()

	f.sync()
	assert.Equal(t, agonesv1.GameServerStateReady, f.state("ready"), "its agent has had no time to dial again")

	f.now = func() time.Time { return noon.Add(29 * time.Second) }
	f.sync()
	assert.Equal(t, agonesv1.GameServerStateReady, f.state("ready"))

	f.now = func() time.Time { return noon.Add(30 * time.Second) }
	f.sync()
	assert.Equal(t, agonesv1.GameServerStateUnhealthy, f.state("ready"), "an agent that is there has said so by now")
	assert.Equal(t, agonesv1.GameServerStateAllocated, f.state("allocated"), "the session is still given a day")

	// A node the operator deleted has no agent to wait for.
	require.NoError(t, f.StartOver())
	require.NoError(t, f.Controller.sync(context.Background(), store.Key{Kind: store.Nodes, Name: "deleted-node"}))
	assert.Equal(t, agonesv1.GameServerStateUnhealthy, f.state("orphan"))
}

func TestTheWaitAfterAStartIsNeverLongerThanTheGrace(t *testing.T) {
	f := newControllerFixture(t)
	f.graces = Graces{Unallocated: time.Second, Allocated: time.Minute}
	f.others(1)
	_, err := f.nodes.Create(disconnectedFor(time.Hour))
	require.NoError(t, err)
	f.add(inState("ready", agonesv1.GameServerStateReady))
	require.NoError(t, f.StartOver())
	f.othersAreBack()

	f.now = func() time.Time { return noon.Add(time.Second) }
	f.sync()

	assert.Equal(t, agonesv1.GameServerStateUnhealthy, f.state("ready"), "whoever sets a grace of a second means it")
}

func TestStartingOverKeepsCountingForANodeThatWasAlreadyGone(t *testing.T) {
	f := newControllerFixture(t)
	f.others(2)
	_, err := f.nodes.Create(disconnectedFor(4 * time.Minute))
	require.NoError(t, err)
	_, err = f.nodes.Create(node("node-2"))
	require.NoError(t, err)
	f.add(inState("ready", agonesv1.GameServerStateReady))

	// A control plane that restarts more often than the grace lasts.
	for minute := 1; minute <= 3; minute++ {
		f.now = func() time.Time { return noon.Add(time.Duration(minute) * time.Minute) }
		require.NoError(t, f.StartOver())
		f.othersAreBack()
	}
	f.now = func() time.Time { return noon.Add(3*time.Minute + warmUp) }
	f.sync()

	assert.Equal(t, agonesv1.GameServerStateUnhealthy, f.state("ready"), "seven minutes out of reach, restarts or not")
	n, _ := f.nodes.Get("", "node-2")
	assert.Equal(t, litev1.NodeDisconnected, n.Status.Phase)
	assert.Equal(t, noon.Add(time.Minute), n.Status.LastHeartbeatTime.UTC(), "a node that was Ready is waited for from the first start on")
}

// Most nodes gone at once is what a broken network at the control plane
// looks like, and then the machines are fine. Their idle GameServers are
// left alone until enough nodes are back to say which it was.
func TestWhileMostNodesAreGoneNothingIsGivenUp(t *testing.T) {
	f := newControllerFixture(t)
	f.others(1)
	_, err := f.nodes.Create(disconnectedFor(10 * time.Minute))
	require.NoError(t, err)
	gone := disconnectedFor(10 * time.Minute)
	gone.Name = "node-2"
	_, err = f.nodes.Create(gone)
	require.NoError(t, err)
	f.add(inState("ready", agonesv1.GameServerStateReady))

	f.sync()
	assert.Equal(t, agonesv1.GameServerStateReady, f.state("ready"), "two of three: it is likelier us than both of them")

	require.NoError(t, f.nodes.Change("", "node-2", func(n *litev1.Node) bool {
		n.Status.Phase = litev1.NodeReady
		return true
	}))
	f.sync()
	assert.Equal(t, agonesv1.GameServerStateUnhealthy, f.state("ready"), "one of three, and gone for ten minutes: that is the node")
}

func TestMostAreGone(t *testing.T) {
	tests := map[string]struct {
		gone, there int
		want        bool
	}{
		"no nodes at all":         {0, 0, false},
		"all there":               {0, 3, false},
		"one of two":              {1, 1, false},
		"half of ten":             {5, 5, false},
		"six of ten":              {6, 4, true},
		"two of three":            {2, 1, true},
		"the only node there is":  {1, 0, true},
		"all of them, at a start": {4, 0, true},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			var nodes []*litev1.Node
			for range test.gone {
				nodes = append(nodes, disconnectedFor(time.Minute))
			}
			for range test.there {
				nodes = append(nodes, node("there"))
			}
			assert.Equal(t, test.want, MostAreGone(nodes))
		})
	}
}

func TestDeletingANodeGivesUpEverythingOnIt(t *testing.T) {
	f := newControllerFixture(t)
	f.add(inState("ready", agonesv1.GameServerStateReady), inState("allocated", agonesv1.GameServerStateAllocated))

	f.sync() // there is no node-1

	assert.Equal(t, agonesv1.GameServerStateUnhealthy, f.state("ready"))
	assert.Equal(t, agonesv1.GameServerStateUnhealthy, f.state("allocated"))
	assert.Contains(t, f.all(), store.Key{Kind: store.Nodes, Name: "node-1"}, "a deleted node is still looked at while servers point to it")
}

func TestDrainShutsDownIdleServersAndKeepsSessions(t *testing.T) {
	f := newControllerFixture(t)
	_, err := f.nodes.Create(node("node-1"))
	require.NoError(t, err)
	f.add(inState("ready", agonesv1.GameServerStateReady), inState("allocated", agonesv1.GameServerStateAllocated))

	require.NoError(t, f.Cordon("node-1", true))
	f.sync()

	assert.Equal(t, agonesv1.GameServerStateShutdown, f.state("ready"))
	assert.Equal(t, agonesv1.GameServerStateAllocated, f.state("allocated"))
	n, _ := f.nodes.Get("", "node-1")
	assert.False(t, n.Schedulable())

	require.NoError(t, f.Uncordon("node-1"))
	n, _ = f.nodes.Get("", "node-1")
	assert.True(t, n.Schedulable())
}
