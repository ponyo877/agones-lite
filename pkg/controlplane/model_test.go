package controlplane

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	pb "agones.dev/agones/pkg/allocation/go"
	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"pgregory.net/rapid"

	"github.com/ponyo877/agones-lite/formal/vectors"
	"github.com/ponyo877/agones-lite/pkg/agones"
	litev1 "github.com/ponyo877/agones-lite/pkg/apis/lite/v1"
	"github.com/ponyo877/agones-lite/pkg/control"
	"github.com/ponyo877/agones-lite/pkg/gameservers"
	"github.com/ponyo877/agones-lite/pkg/nodes"
	"github.com/ponyo877/agones-lite/pkg/store"
)

// This is a model test of the whole control plane.
//
// rapid draws a history. Operators apply and delete things, matchmakers
// allocate, nodes come and go and report, time passes, the control plane
// restarts. Between any two of those, rapid also draws which controller
// looks at which resource next: the order that goroutines would only find
// by luck. After every step the test checks what must hold at all times.
// Now and then it lets everything come to rest and checks what must hold
// then.
//
// Nothing in this world moves by itself. There is no goroutine and no
// timer, the clock is the test's, and so are the names the store would draw
// at random. The same draws give the same history, so rapid can shrink a
// failure to a few steps and replay it.

// Sessions on a node that fell silent are kept for an hour here, and its
// other game servers for half a minute.
var modelGraces = nodes.Graces{Unallocated: 30 * time.Second, Allocated: time.Hour}

func TestTheControlPlaneKeepsItsPromisesWhateverHappensInWhateverOrder(t *testing.T) {
	require.NoError(t, agones.Setup())
	moves := movesInTheTable(t)
	rapid.Check(t, func(t *rapid.T) {
		w := newModelWorld(t, moves)
		t.Repeat(map[string]func(*rapid.T){
			"an operator applies a Fleet":           w.applyFleet,
			"an operator deletes a Fleet":           w.deleteFleet,
			"an operator applies a FleetAutoscaler": w.applyAutoscaler,
			"an operator deletes a FleetAutoscaler": w.deleteAutoscaler,
			"an operator creates a GameServer":      w.createGameServer,
			"an operator deletes a GameServer":      w.deleteGameServer,
			"an operator cordons or drains a node":  w.operateNode,
			"an operator deletes a node":            w.deleteNode,
			"a matchmaker allocates":                w.allocate,
			"a node joins":                          w.join,
			"a node falls silent":                   w.fallSilent,
			"a node comes back":                     w.comeBack,
			"a node does its work":                  w.work,
			"a node reports something":              w.report,
			"controllers look at things":            w.step,
			"time passes":                           w.wait,
			"the control plane restarts":            w.restart,
			"everything comes to rest":              w.rest,
			"":                                      w.look,
		})
	})
}

// modelWorld is a control plane in which nothing moves by itself.
type modelWorld struct {
	t     *rapid.T
	path  string    // the store's file, which outlives a restart
	now   time.Time // the only clock
	names int       // how many names the store was given
	moves map[move]bool

	store   *store.Store
	cp      *ControlPlane
	api     http.Handler
	stepper *control.Stepper
	servers <-chan store.Event // every version of every GameServer
	unwatch func()

	puppets map[string]*puppet
	seen    map[types.UID]*agonesv1.GameServer // each GameServer as look last saw it
	excused map[types.UID]string               // sessions the test itself put an end to, and how
}

// puppet is a node whose every report the test makes.
type puppet struct {
	node        *litev1.Node
	connected   bool
	silentSince time.Time
	containers  map[types.UID]bool // the GameServers whose containers are here
}

// move is a change of state.
type move struct{ from, to agonesv1.GameServerState }

// movesInTheTable lists the changes of state that some event makes. They
// are read from the Lean model's copy of the table, which a test in
// pkg/gameservers holds against the Go one.
func movesInTheTable(t *testing.T) map[move]bool {
	type cell struct {
		State string  `json:"state"`
		Next  *string `json:"next"`
	}
	moves := map[move]bool{}
	for _, c := range vectors.Read[cell](t, "transitions.jsonl") {
		if c.Next != nil {
			moves[move{agonesv1.GameServerState(c.State), agonesv1.GameServerState(*c.Next)}] = true
		}
	}
	return moves
}

func newModelWorld(t *rapid.T, moves map[move]bool) *modelWorld {
	dir, err := os.MkdirTemp("", "model")
	require.NoError(t, err)
	w := &modelWorld{
		t:       t,
		path:    filepath.Join(dir, "state.db"),
		now:     time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		moves:   moves,
		puppets: map[string]*puppet{},
		seen:    map[types.UID]*agonesv1.GameServer{},
		excused: map[types.UID]string{},
	}
	w.open()
	t.Cleanup(func() {
		w.close()
		_ = os.RemoveAll(dir)
	})
	return w
}

// open starts a control plane on the store's file: the first time, and
// again after every restart.
func (w *modelWorld) open() {
	s, err := store.Open(w.path, store.Kinds())
	require.NoError(w.t, err)
	s.Clock, s.Suffix = w.clock, w.name
	cp, err := New(s, nil, nil, modelGraces)
	require.NoError(w.t, err)
	cp.Allocation.Pick = func(n int) int { return rapid.IntRange(0, n-1).Draw(w.t, "pick") }
	w.store, w.cp, w.api = s, cp, cp.API.Handler()
	w.stepper = &control.Stepper{}
	controllers := cp.controllers()
	for _, name := range slices.Sorted(maps.Keys(controllers)) {
		w.stepper.Take(name, controllers[name])
	}
	w.servers, w.unwatch = store.GameServersOf(s).Watch()
}

func (w *modelWorld) close() {
	w.unwatch()
	w.stepper.Close()
	require.NoError(w.t, w.store.Close())
}

func (w *modelWorld) clock() time.Time { return w.now }

// name stands in for the five random characters at the end of a generated name.
func (w *modelWorld) name() string {
	w.names++
	return fmt.Sprintf("%05d", w.names)
}

// ---- What operators and matchmakers do.

// operator sends a request to the management API, as the CLI does. Whatever
// is asked, the API must not fall over.
func (w *modelWorld) operator(method, path, body string) {
	rec := httptest.NewRecorder()
	w.api.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
	require.Less(w.t, rec.Code, 500, "%s %s: %s", method, path, rec.Body.String())
}

func aFleetName() *rapid.Generator[string] {
	return rapid.SampledFrom([]string{"red", "blue"})
}

const modelFleet = `
apiVersion: agones.dev/v1
kind: Fleet
metadata:
  name: %s
spec:
  replicas: %d
  scheduling: %s
  strategy:
    type: %s
  template:
    spec:
      ports:
        - name: default
          containerPort: 7654
      template:
        spec:
          containers:
            - name: game
              image: example/game:%d
`

func (w *modelWorld) applyFleet(t *rapid.T) {
	name := aFleetName().Draw(t, "fleet")
	body := fmt.Sprintf(modelFleet, name,
		rapid.IntRange(0, 4).Draw(t, "replicas"),
		rapid.SampledFrom([]string{"Packed", "Distributed"}).Draw(t, "scheduling"),
		rapid.SampledFrom([]string{"RollingUpdate", "Recreate"}).Draw(t, "strategy"),
		rapid.IntRange(1, 3).Draw(t, "version"))
	w.operator(http.MethodPut, "/v1/namespaces/default/fleets/"+name, body)
}

func (w *modelWorld) deleteFleet(t *rapid.T) {
	name := aFleetName().Draw(t, "fleet")
	w.excuse("its Fleet was deleted", w.serversOf(name)...)
	w.operator(http.MethodDelete, "/v1/namespaces/default/fleets/"+name, "")
}

const modelAutoscaler = `
apiVersion: autoscaling.agones.dev/v1
kind: FleetAutoscaler
metadata:
  name: %s
spec:
  fleetName: %s
  policy:
    type: Buffer
    buffer:
      bufferSize: %d
      minReplicas: %d
      maxReplicas: %d
`

func (w *modelWorld) applyAutoscaler(t *rapid.T) {
	name := aFleetName().Draw(t, "fleet")
	buffer := rapid.IntRange(1, 2).Draw(t, "buffer")
	body := fmt.Sprintf(modelAutoscaler, name, name, buffer,
		rapid.IntRange(buffer, 3).Draw(t, "min"), rapid.IntRange(3, 6).Draw(t, "max"))
	w.operator(http.MethodPut, "/v1/namespaces/default/fleetautoscalers/"+name, body)
}

func (w *modelWorld) deleteAutoscaler(t *rapid.T) {
	w.operator(http.MethodDelete, "/v1/namespaces/default/fleetautoscalers/"+aFleetName().Draw(t, "fleet"), "")
}

const modelGameServer = `
apiVersion: agones.dev/v1
kind: GameServer
metadata:
  generateName: lone-
spec:
  ports:
    - name: default
      containerPort: 7654
  template:
    spec:
      containers:
        - name: game
          image: example/game:1
`

func (w *modelWorld) createGameServer(*rapid.T) {
	w.operator(http.MethodPost, "/v1/namespaces/default/gameservers", modelGameServer)
}

func (w *modelWorld) deleteGameServer(t *rapid.T) {
	gs := w.aGameServer(t, w.all())
	w.excuse("it was deleted", gs)
	w.operator(http.MethodDelete, "/v1/namespaces/default/gameservers/"+gs.Name, "")
}

func (w *modelWorld) operateNode(t *rapid.T) {
	p := w.aPuppet(t, func(*puppet) bool { return true })
	operation := rapid.SampledFrom([]string{"cordon", "drain", "uncordon"}).Draw(t, "operation")
	w.operator(http.MethodPost, "/v1/nodes/"+p.node.Name+"/"+operation, "")
}

// A deleted node is a machine that is not coming back.
func (w *modelWorld) deleteNode(t *rapid.T) {
	p := w.aPuppet(t, func(p *puppet) bool { return !p.connected })
	w.excuse("its node was deleted", w.on(p)...)
	w.operator(http.MethodDelete, "/v1/nodes/"+p.node.Name, "")
	delete(w.puppets, p.node.Name)
}

func (w *modelWorld) allocate(t *rapid.T) {
	_, err := w.cp.Allocation.Allocate(context.Background(), &pb.AllocationRequest{
		Namespace: "default",
		GameServerSelectors: []*pb.GameServerSelector{
			{MatchLabels: map[string]string{agonesv1.FleetNameLabel: aFleetName().Draw(t, "fleet")}},
		},
	})
	if err != nil {
		require.Equal(t, codes.ResourceExhausted, status.Code(err), "the only way not to get one is that there is none: %v", err)
	}
}

// ---- What nodes do.

func (w *modelWorld) join(t *rapid.T) {
	name := rapid.SampledFrom([]string{"node-1", "node-2", "node-3"}).Draw(t, "node")
	if w.puppets[name] != nil {
		t.Skip("it is here already")
	}
	p := &puppet{containers: map[types.UID]bool{}, node: &litev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: litev1.NodeStatus{
			Phase:     litev1.NodeReady,
			Address:   "203.0.113." + name[len(name)-1:],
			PortRange: litev1.PortRange{Min: 7000, Max: 7999},
		},
	}}
	w.puppets[name] = p
	w.connect(p)
}

func (w *modelWorld) connect(p *puppet) {
	require.NoError(w.t, w.cp.Agents.Register(context.Background(), p.node))
	p.connected = true
}

// fallSilent is what the control plane sees when an agent's connection
// ends: see Server.connected in pkg/nodes. Sometimes the machine is gone
// with it, and its containers too.
func (w *modelWorld) fallSilent(t *rapid.T) {
	p := w.aPuppet(t, func(p *puppet) bool { return p.connected })
	w.silence(p)
	if rapid.Bool().Draw(t, "the machine died") {
		clear(p.containers)
	}
}

func (w *modelWorld) silence(p *puppet) {
	p.connected, p.silentSince = false, w.now
	_ = store.NodesOf(w.store).Change("", p.node.Name, func(n *litev1.Node) bool {
		n.Status.Phase = litev1.NodeDisconnected
		n.Status.LastHeartbeatTime = metav1.NewTime(w.now)
		return true
	})
}

func (w *modelWorld) comeBack(t *rapid.T) {
	w.connect(w.aPuppet(t, func(p *puppet) bool { return !p.connected }))
}

// work lets a node do the next thing a healthy node does for one of its
// GameServers.
func (w *modelWorld) work(t *rapid.T) {
	p := w.aPuppet(t, func(p *puppet) bool { return p.connected })
	gs := w.aGameServer(t, w.on(p))
	if event, ok := p.next(gs); ok {
		w.say(p, gs, event)
	}
}

// next is what a node on which all goes well reports next about gs.
func (p *puppet) next(gs *agonesv1.GameServer) (gameservers.Event, bool) {
	here := p.containers[gs.UID]
	switch {
	case gs.DeletionTimestamp != nil:
		return gameservers.ContainersRemoved, true
	case gameservers.Final(gs.Status.State):
		return "", false
	case gs.Status.State == agonesv1.GameServerStateCreating:
		return gameservers.ContainersCreated, true
	case !here:
		return gameservers.ContainersMissing, true // the machine died meanwhile
	case gs.Status.State == agonesv1.GameServerStateStarting:
		return gameservers.ContainersRunning, true
	case gs.Status.State == agonesv1.GameServerStateScheduled:
		return gameservers.SDKReady, true
	}
	return "", false
}

// Everything an agent may report: what its containers do, and what the
// game server asks through the SDK.
var reportable = []gameservers.Event{
	gameservers.ContainersCreated, gameservers.CreateFailed, gameservers.ContainersRunning,
	gameservers.ExitedCleanly, gameservers.ExitedWithError,
	gameservers.ContainersMissing, gameservers.ContainersRemoved,
	gameservers.SDKReady, gameservers.SDKAllocate, gameservers.SDKReserve,
	gameservers.SDKShutdown, gameservers.HealthFailed, gameservers.ReserveExpired,
}

// report lets a node say anything about one of its GameServers, in or out
// of turn. Game servers crash, call the SDK early, and agents repeat
// themselves; the control plane has to make sense of all of it.
func (w *modelWorld) report(t *rapid.T) {
	p := w.aPuppet(t, func(p *puppet) bool { return p.connected })
	gs := w.aGameServer(t, w.on(p))
	w.say(p, gs, rapid.SampledFrom(reportable).Draw(t, "event"))
}

// say makes the report. One that may end a session is the game server's
// own doing, or its node's, and so excuses the end.
func (w *modelWorld) say(p *puppet, gs *agonesv1.GameServer, event gameservers.Event) {
	if _, ends := gameservers.Next(agonesv1.GameServerStateAllocated, event); ends {
		w.excuse("its node reported "+string(event), gs)
	}
	switch event {
	case gameservers.ContainersCreated:
		p.containers[gs.UID] = true
	case gameservers.ContainersRemoved, gameservers.ContainersMissing:
		delete(p.containers, gs.UID)
	}
	err := w.cp.Agents.Report(context.Background(), gameservers.Report{Namespace: gs.Namespace, Name: gs.Name, Event: event})
	require.NoError(w.t, err, "reporting %s for %s", event, gs.Name)
}

// ---- What the control plane does, and time.

// step lets controllers look at a few of the things that wait for them.
func (w *modelWorld) step(t *rapid.T) {
	for range rapid.IntRange(1, 8).Draw(t, "steps") {
		pending := w.stepper.Pending()
		if len(pending) == 0 {
			return
		}
		w.run(pending[rapid.IntRange(0, len(pending)-1).Draw(t, "which")])
	}
}

func (w *modelWorld) run(step control.Step) {
	require.NoError(w.t, w.stepper.Run(context.Background(), step), "%s", step)
}

// wait moves the clock. Controllers look at everything again every few
// seconds, so they do here.
func (w *modelWorld) wait(t *rapid.T) {
	passed := rapid.SampledFrom([]time.Duration{time.Second, 40 * time.Second, 2 * time.Hour}).Draw(t, "passed")
	w.now = w.now.Add(passed)
	w.stepper.Resync()
	for _, p := range w.puppets {
		if !p.connected && w.now.Sub(p.silentSince) >= modelGraces.Allocated {
			w.excuse("its node was silent for too long", w.on(p)...)
		}
	}
}

// restart ends the control plane and starts another on the same file. The
// nodes are all there still, but have to connect again.
func (w *modelWorld) restart(*rapid.T) {
	w.close()
	w.open()
	for _, p := range w.puppets {
		if p.connected {
			p.connected, p.silentSince = false, w.now
		}
	}
}

// ---- What must hold at all times.

func (w *modelWorld) look(*rapid.T) {
	w.follow()
	w.hostPortsDoNotClash()
	w.playableServersCanBeReached()
	w.sizesAreNotNegative()
}

// follow reads every version of every GameServer since the last look and
// holds each against the one before.
func (w *modelWorld) follow() {
	for {
		select {
		case e := <-w.servers:
			gs := e.Object.(*agonesv1.GameServer)
			w.compare(w.seen[gs.UID], gs, e.Type == store.Deleted)
			w.seen[gs.UID] = gs
			if e.Type == store.Deleted {
				delete(w.seen, gs.UID)
			}
		default:
			return
		}
	}
}

func (w *modelWorld) compare(before, after *agonesv1.GameServer, gone bool) {
	if before == nil {
		return
	}
	from, to := before.Status.State, after.Status.State
	if from != to {
		require.True(w.t, w.moves[move{from, to}], "%s went from %s to %s, which no event does", after.Name, from, to)
	}
	if before.DeletionTimestamp != nil && !gone {
		require.NotNil(w.t, after.DeletionTimestamp, "%s was being deleted, and then was not", after.Name)
	}
	inSession := from == agonesv1.GameServerStateAllocated && before.DeletionTimestamp == nil
	still := to == agonesv1.GameServerStateAllocated && after.DeletionTimestamp == nil && !gone
	if inSession && !still {
		require.NotEmpty(w.t, w.excused[after.UID],
			"%s had players on it and was taken away (now %s, deleting: %v, gone: %v) though nothing asked for that",
			after.Name, to, after.DeletionTimestamp != nil, gone)
	}
}

func (w *modelWorld) hostPortsDoNotClash() {
	type place struct {
		node     string
		port     int32
		protocol string
	}
	taken := map[place]string{}
	for _, gs := range w.all() {
		for _, p := range gs.Spec.Ports {
			if gs.Status.NodeName == "" || p.HostPort == 0 {
				continue
			}
			at := place{gs.Status.NodeName, p.HostPort, string(p.Protocol)}
			require.Empty(w.t, taken[at], "%s and %s both have port %d on %s", gs.Name, taken[at], p.HostPort, at.node)
			taken[at] = gs.Name
		}
	}
}

// A GameServer that players may be sent to says where it is.
func (w *modelWorld) playableServersCanBeReached() {
	for _, gs := range w.all() {
		switch gs.Status.State {
		case agonesv1.GameServerStateReady, agonesv1.GameServerStateAllocated, agonesv1.GameServerStateReserved:
			require.NotEmpty(w.t, gs.Status.Address, "%s is %s without an address", gs.Name, gs.Status.State)
			require.Len(w.t, gs.Status.Ports, len(gs.Spec.Ports), "%s is %s without its ports", gs.Name, gs.Status.State)
		}
	}
}

func (w *modelWorld) sizesAreNotNegative() {
	for _, fleet := range store.FleetsOf(w.store).List("") {
		require.GreaterOrEqual(w.t, fleet.Spec.Replicas, int32(0), "Fleet %s", fleet.Name)
	}
	for _, set := range store.GameServerSetsOf(w.store).List("") {
		require.GreaterOrEqual(w.t, set.Spec.Replicas, int32(0), "GameServerSet %s", set.Name)
	}
}

// ---- What must hold once everything has come to rest.

// rest brings every node back, lets the nodes and the controllers work
// until none of them has anything left to do, and then looks at the result.
func (w *modelWorld) rest(t *rapid.T) {
	w.everyNodeIsBackAndOpen(t)
	for round := 0; w.busy(); round++ {
		require.Less(t, round, 500, "the control plane never comes to rest")
	}
	w.look(t)
	w.nothingIsLeftHalfDone()
	for _, fleet := range store.FleetsOf(w.store).List("") {
		w.fleetHasWhatItAskedFor(fleet)
	}
}

func (w *modelWorld) everyNodeIsBackAndOpen(t *rapid.T) {
	if len(w.puppets) == 0 {
		w.join(t)
	}
	for _, name := range slices.Sorted(maps.Keys(w.puppets)) {
		p := w.puppets[name]
		if !p.connected {
			w.connect(p)
		}
		w.operator(http.MethodPost, "/v1/nodes/"+name+"/uncordon", "")
	}
}

// busy does one round of everything that waits, and reports whether there
// was anything.
func (w *modelWorld) busy() bool {
	w.runAll()
	if w.nodesWork() || len(w.stepper.Pending()) > 0 {
		return true
	}
	// Nothing waits. Would a look at everything, a minute later, find
	// something to do? A GameServer in Error, for one, is only replaced
	// after a while.
	w.now = w.now.Add(time.Minute)
	w.stepper.Resync()
	return w.changedBy(w.runAll)
}

// runAll lets the controllers look at everything that waits right now.
func (w *modelWorld) runAll() {
	for _, step := range w.stepper.Pending() {
		w.run(step)
	}
}

// nodesWork lets every node do what a healthy node does next for each of
// its GameServers, and reports whether any had something to do.
func (w *modelWorld) nodesWork() bool {
	worked := false
	for _, name := range slices.Sorted(maps.Keys(w.puppets)) {
		p := w.puppets[name]
		for _, gs := range w.on(p) {
			if event, ok := p.next(gs); ok {
				w.say(p, gs, event)
				worked = true
			}
		}
	}
	return worked
}

// changedBy reports whether do changed any GameServer.
func (w *modelWorld) changedBy(do func()) bool {
	w.follow()
	do()
	before := len(w.servers)
	w.follow()
	return before > 0
}

func (w *modelWorld) nothingIsLeftHalfDone() {
	for _, gs := range w.all() {
		require.Nil(w.t, gs.DeletionTimestamp, "%s is still being deleted", gs.Name)
	}
	for _, set := range store.GameServerSetsOf(w.store).List("") {
		require.Nil(w.t, set.DeletionTimestamp, "GameServerSet %s is still being deleted", set.Name)
	}
	for _, fleet := range store.FleetsOf(w.store).List("") {
		require.Nil(w.t, fleet.DeletionTimestamp, "Fleet %s is still being deleted", fleet.Name)
	}
	for _, scaler := range store.FleetAutoscalersOf(w.store).List("") {
		require.Nil(w.t, scaler.DeletionTimestamp, "FleetAutoscaler %s is still being deleted", scaler.Name)
	}
}

// A Fleet at rest has as many game servers as it asked for, all of them
// usable, the Ready ones of the version it asked for, and it says so.
func (w *modelWorld) fleetHasWhatItAskedFor(fleet *agonesv1.Fleet) {
	count := map[agonesv1.GameServerState]int32{}
	for _, gs := range w.serversOf(fleet.Name) {
		count[gs.Status.State]++
	}
	ready := count[agonesv1.GameServerStateReady]
	sessions := count[agonesv1.GameServerStateAllocated] + count[agonesv1.GameServerStateReserved]
	total := int32(len(w.serversOf(fleet.Name)))

	require.Equal(w.t, total, ready+sessions, "Fleet %s has game servers that are neither Ready nor in use: %v", fleet.Name, count)
	require.GreaterOrEqual(w.t, total, fleet.Spec.Replicas, "Fleet %s has fewer game servers than it asked for: %v", fleet.Name, count)
	require.LessOrEqual(w.t, ready, fleet.Spec.Replicas, "Fleet %s has more Ready game servers than it asked for: %v", fleet.Name, count)
	require.Equal(w.t, total, fleet.Status.Replicas, "Fleet %s: status.replicas", fleet.Name)
	require.Equal(w.t, ready, fleet.Status.ReadyReplicas, "Fleet %s: status.readyReplicas", fleet.Name)
	require.Equal(w.t, count[agonesv1.GameServerStateAllocated], fleet.Status.AllocatedReplicas, "Fleet %s: status.allocatedReplicas", fleet.Name)

	for _, set := range store.GameServerSetsOf(w.store).List(fleet.Namespace) {
		old := !apiequality.Semantic.DeepEqual(set.Spec.Template, fleet.Spec.Template)
		if metav1.IsControlledBy(set, fleet) && old {
			require.Zero(w.t, set.Status.ReadyReplicas, "the old GameServerSet %s still has Ready game servers", set.Name)
		}
	}
}

// ---- Looking things up.

func (w *modelWorld) all() []*agonesv1.GameServer {
	all := store.GameServersOf(w.store).List("")
	slices.SortFunc(all, func(a, b *agonesv1.GameServer) int { return strings.Compare(a.Name, b.Name) })
	return all
}

func (w *modelWorld) serversOf(fleet string) []*agonesv1.GameServer {
	return slices.DeleteFunc(w.all(), func(gs *agonesv1.GameServer) bool {
		return gs.Labels[agonesv1.FleetNameLabel] != fleet
	})
}

func (w *modelWorld) on(p *puppet) []*agonesv1.GameServer {
	return slices.DeleteFunc(w.all(), func(gs *agonesv1.GameServer) bool {
		return gs.Status.NodeName != p.node.Name
	})
}

func (w *modelWorld) excuse(why string, servers ...*agonesv1.GameServer) {
	for _, gs := range servers {
		w.excused[gs.UID] = why
	}
}

// aGameServer draws one of the given GameServers, or skips the step when
// there is none.
func (w *modelWorld) aGameServer(t *rapid.T, from []*agonesv1.GameServer) *agonesv1.GameServer {
	if len(from) == 0 {
		t.Skip("there is no such GameServer")
	}
	return from[rapid.IntRange(0, len(from)-1).Draw(t, "gameserver")]
}

// aPuppet draws a node that fits, or skips the step when there is none.
func (w *modelWorld) aPuppet(t *rapid.T, fits func(*puppet) bool) *puppet {
	var names []string
	for name, p := range w.puppets {
		if fits(p) {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		t.Skip("there is no such node")
	}
	slices.Sort(names)
	return w.puppets[rapid.SampledFrom(names).Draw(t, "node")]
}
