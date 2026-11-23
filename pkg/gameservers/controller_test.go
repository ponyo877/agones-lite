package gameservers

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ponyo877/agones-lite/pkg/agones"
	litev1 "github.com/ponyo877/agones-lite/pkg/apis/lite/v1"
	"github.com/ponyo877/agones-lite/pkg/events"
	"github.com/ponyo877/agones-lite/pkg/store"
)

var noon = time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)

// fixture is a Controller on an empty store, driven by hand instead of Run.
type fixture struct {
	t *testing.T
	*Controller
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	require.NoError(t, agones.Setup())
	s, err := store.Open(filepath.Join(t.TempDir(), "state.db"), store.Kinds())
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	c := NewController(s, nil)
	c.now = func() time.Time { return noon }
	return fixture{t: t, Controller: c}
}

func (f fixture) addNode() {
	f.t.Helper()
	_, err := f.nodes.Create(&litev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
		Status: litev1.NodeStatus{
			Phase:     litev1.NodeReady,
			Address:   "203.0.113.10",
			PortRange: litev1.PortRange{Min: 7000, Max: 8000},
		},
	})
	require.NoError(f.t, err)
}

// addServer creates the GameServer the way the API does: defaults first.
func (f fixture) addServer() {
	f.t.Helper()
	gs := &agonesv1.GameServer{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "gs"}}
	gs.Spec.Ports = []agonesv1.GameServerPort{{Name: "game", ContainerPort: 7654}}
	gs.Spec.Template.Spec.Containers = []corev1.Container{{Name: "game", Image: "example/game"}}
	gs.ApplyDefaults()
	_, err := f.servers.Create(gs)
	require.NoError(f.t, err)
}

func (f fixture) sync() {
	f.t.Helper()
	require.NoError(f.t, f.Controller.sync(context.Background(), store.Key{Kind: store.GameServers, Namespace: "default", Name: "gs"}))
}

func (f fixture) report(e Event) {
	f.t.Helper()
	require.NoError(f.t, f.Report(Report{Namespace: "default", Name: "gs", Event: e}))
}

func (f fixture) server() *agonesv1.GameServer {
	f.t.Helper()
	gs, ok := f.servers.Get("default", "gs")
	require.True(f.t, ok, "the GameServer should exist")
	return gs
}

func TestFromCreationToReady(t *testing.T) {
	f := newFixture(t)
	f.addNode()
	f.addServer()
	assert.Equal(t, portAllocation, f.server().Status.State)

	f.sync()
	gs := f.server()
	assert.Equal(t, creating, gs.Status.State)
	assert.Equal(t, "node-1", gs.Status.NodeName)
	assert.Equal(t, int32(7000), gs.Spec.Ports[0].HostPort)
	assert.Empty(t, gs.Status.Address, "not reachable before the containers run")

	f.report(ContainersCreated)
	f.report(ContainersRunning)
	gs = f.server()
	assert.Equal(t, scheduled, gs.Status.State)
	assert.Equal(t, "203.0.113.10", gs.Status.Address)
	assert.Equal(t, []agonesv1.GameServerStatusPort{{Name: "game", Port: 7000}}, gs.Status.Ports)

	f.report(SDKReady)
	assert.Equal(t, requestReady, f.server().Status.State)

	f.sync()
	assert.Equal(t, ready, f.server().Status.State)
}

func TestReadyBeforeTheAgentReportsRunning(t *testing.T) {
	f := newFixture(t)
	f.addNode()
	f.addServer()
	f.sync()
	f.report(ContainersCreated)

	f.report(SDKReady)
	f.sync()
	f.report(ContainersRunning) // arrives late and must change nothing

	gs := f.server()
	assert.Equal(t, ready, gs.Status.State)
	assert.Equal(t, "203.0.113.10", gs.Status.Address, "Ready publishes the address when Scheduled never happened")
}

// A game server may call Allocate or Reserve as soon as it starts, before
// its agent reports it running. Upstream has an answer for Ready only.
func TestAllocatedOrReservedBeforeTheAgentReportsRunning(t *testing.T) {
	tests := map[string]struct {
		event Event
		want  agonesv1.GameServerState
	}{
		"Allocate": {event: SDKAllocate, want: allocated},
		"Reserve":  {event: SDKReserve, want: reserved},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.addNode()
			f.addServer()
			f.sync()
			f.report(ContainersCreated)

			f.report(tc.event)
			f.report(ContainersRunning) // arrives late and must change nothing

			gs := f.server()
			assert.Equal(t, tc.want, gs.Status.State)
			assert.Equal(t, "203.0.113.10", gs.Status.Address, "players are sent here, so they must be told where it is")
			assert.Equal(t, []agonesv1.GameServerStatusPort{{Name: "game", Port: 7000}}, gs.Status.Ports)
		})
	}
}

func TestWaitsForANodeWithRoom(t *testing.T) {
	f := newFixture(t)
	f.addServer()

	f.sync()
	assert.Equal(t, portAllocation, f.server().Status.State)
	assert.Empty(t, f.server().Status.NodeName)

	f.addNode()
	f.sync()
	assert.Equal(t, "node-1", f.server().Status.NodeName)
}

func TestShutdownRemovesTheGameServer(t *testing.T) {
	f := newFixture(t)
	f.addNode()
	f.addServer()
	f.sync()
	f.report(ContainersCreated)
	f.report(ContainersRunning)

	f.report(SDKShutdown)
	assert.Equal(t, shutdown, f.server().Status.State)

	f.sync()
	assert.NotNil(t, f.server().DeletionTimestamp, "the record stays until the containers are gone")

	f.report(SDKReady)
	assert.Equal(t, shutdown, f.server().Status.State, "nothing changes once deletion has begun")

	f.report(ContainersRemoved)
	_, ok := f.servers.Get("default", "gs")
	assert.False(t, ok)
}

func TestDeletingBeforePlacementNeedsNoCleanup(t *testing.T) {
	f := newFixture(t)
	f.addServer()

	require.NoError(t, f.Delete("default", "gs"))
	f.sync()

	_, ok := f.servers.Get("default", "gs")
	assert.False(t, ok)
}

func TestContainersRemovedIsIgnoredUnlessDeleting(t *testing.T) {
	f := newFixture(t)
	f.addNode()
	f.addServer()
	f.sync()

	f.report(ContainersRemoved)

	assert.Equal(t, creating, f.server().Status.State)
}

func TestReserveAndAllocateRecordTheirTime(t *testing.T) {
	f := newFixture(t)
	f.addNode()
	f.addServer()
	f.sync()
	f.report(ContainersCreated)
	f.report(ContainersRunning)

	require.NoError(t, f.Report(Report{Namespace: "default", Name: "gs", Event: SDKReserve, Reserve: time.Minute}))
	gs := f.server()
	assert.Equal(t, reserved, gs.Status.State)
	assert.Equal(t, noon.Add(time.Minute), gs.Status.ReservedUntil.UTC())

	f.report(SDKAllocate)
	gs = f.server()
	assert.Equal(t, allocated, gs.Status.State)
	assert.Nil(t, gs.Status.ReservedUntil, "leaving Reserved clears the deadline")
	assert.Equal(t, "2026-11-23T12:00:00Z", gs.Annotations[LastAllocatedAnnotation])
}

// The sidecar's timer may fire just as the game server allocates itself,
// and what the timer says may then arrive after the allocation.
func TestAReservationThatRunsOutDoesNotEndASession(t *testing.T) {
	f := newFixture(t)
	f.addNode()
	f.addServer()
	f.sync()
	f.report(ContainersCreated)
	f.report(ContainersRunning)

	f.report(SDKReserve)
	f.report(SDKAllocate)
	f.report(ReserveExpired)
	f.sync()
	assert.Equal(t, allocated, f.server().Status.State, "the session goes on")

	// Without a session in the way, the server goes back to the pool.
	f.report(SDKReserve)
	f.report(ReserveExpired)
	f.sync()
	assert.Equal(t, ready, f.server().Status.State)
}

// history gives the fixture an event history to write to and returns what
// the latest line about the GameServer says.
func (f fixture) history() (latest func() string) {
	f.t.Helper()
	recorder, err := events.Open(filepath.Join(f.t.TempDir(), "events.db"))
	require.NoError(f.t, err)
	f.t.Cleanup(func() { _ = recorder.Close() })
	f.events = recorder
	return func() string {
		lines, err := recorder.For(Kind, "default", "gs")
		require.NoError(f.t, err)
		require.NotEmpty(f.t, lines)
		return lines[0].Message
	}
}

func TestAReportsDetailIsWrittenIntoTheHistoryOnOneLine(t *testing.T) {
	f := newFixture(t)
	latest := f.history()
	f.addNode()
	f.addServer()
	f.sync()

	f.report(ContainersCreated)
	assert.Equal(t, "ContainersCreated: was Creating", latest(), "without a detail the line is as short as before")

	require.NoError(t, f.Report(Report{
		Namespace: "default", Name: "gs", Event: CreateFailed,
		Detail: "start gs_game: docker: driver failed programming external connectivity:\n\tBind for 0.0.0.0:7000 failed: port is already allocated (HTTP 500)\n",
	}))
	assert.Equal(t, "CreateFailed: was Starting: start gs_game: docker: driver failed programming external connectivity: "+
		"Bind for 0.0.0.0:7000 failed: port is already allocated (HTTP 500)", latest())
}

func TestALongDetailIsCutInTheMiddle(t *testing.T) {
	f := newFixture(t)
	latest := f.history()
	f.addNode()
	f.addServer()
	f.sync()

	rambling := "pull example/game:1: " + strings.Repeat("layer after layer ", 1000) + "denied: no access"
	require.NoError(t, f.Report(Report{Namespace: "default", Name: "gs", Event: CreateFailed, Detail: rambling}))

	line := latest()
	assert.Less(t, len(line), 400)
	assert.Contains(t, line, "CreateFailed: was Creating: pull example/game:1: ", "an error names what was tried first")
	assert.True(t, strings.HasSuffix(line, "denied: no access"), "and the cause last")
}

// A GameServer was deleted and another made under its name. An agent that
// was cut off meanwhile still reports about the first one.
func TestAReportAboutAnEarlierGameServerOfTheSameNameIsIgnored(t *testing.T) {
	f := newFixture(t)
	f.addNode()
	f.addServer()
	f.sync()
	current := f.server().UID
	require.NotEmpty(t, current)

	for _, event := range []Event{ContainersCreated, CreateFailed, SDKShutdown} {
		require.NoError(t, f.Report(Report{Namespace: "default", Name: "gs", UID: "of-an-earlier-one", Event: event}))
	}
	assert.Equal(t, agonesv1.GameServerStateCreating, f.server().Status.State, "none of that was about this GameServer")

	require.NoError(t, f.Report(Report{Namespace: "default", Name: "gs", UID: current, Event: ContainersCreated}))
	assert.Equal(t, agonesv1.GameServerStateStarting, f.server().Status.State)

	// Nor do the earlier one's containers going away remove this one.
	require.NoError(t, f.Delete("default", "gs"))
	require.NoError(t, f.Report(Report{Namespace: "default", Name: "gs", UID: "of-an-earlier-one", Event: ContainersRemoved}))
	assert.NotNil(t, f.server(), "still waiting for its own containers to be removed")
}

func TestDecide(t *testing.T) {
	deleted := metav1.NewTime(noon)
	tests := map[string]struct {
		state    agonesv1.GameServerState
		node     string
		deleted  bool
		nodeGone bool
		want     step
	}{
		"new, needs ports":            {state: portAllocation, want: place},
		"new, no ports to allocate":   {state: creating, want: place},
		"placed, agent's turn":        {state: creating, node: "n", want: wait},
		"asked to be ready":           {state: requestReady, node: "n", want: grantReady},
		"ready":                       {state: ready, node: "n", want: wait},
		"shut down":                   {state: shutdown, node: "n", want: startDelete},
		"deleting, containers exist":  {state: shutdown, node: "n", deleted: true, want: wait},
		"deleting, never placed":      {state: creating, deleted: true, want: forget},
		"deleting, node out of reach": {state: shutdown, node: "n", deleted: true, nodeGone: true, want: forget},
		"failed before placement":     {state: failed, want: wait},
		"unhealthy stays for a look":  {state: unhealthy, node: "n", want: wait},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			gs := &agonesv1.GameServer{}
			gs.Status.State = tc.state
			gs.Status.NodeName = tc.node
			if tc.deleted {
				gs.DeletionTimestamp = &deleted
			}
			assert.Equal(t, tc.want, decide(gs, !tc.nodeGone))
		})
	}
}

func TestSetMetadataAddsThePrefix(t *testing.T) {
	f := newFixture(t)
	f.addServer()

	require.NoError(t, f.SetMetadata("default", "gs", map[string]string{"mode": "ranked"}, map[string]string{"map": "dust"}))

	gs := f.server()
	assert.Equal(t, "ranked", gs.Labels["agones.dev/sdk-mode"])
	assert.Equal(t, "dust", gs.Annotations["agones.dev/sdk-map"])
}
