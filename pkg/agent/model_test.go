package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"pgregory.net/rapid"

	scrimv1 "github.com/ponyo877/scrim/pkg/apis/scrim/v1"
	"github.com/ponyo877/scrim/pkg/docker"
	"github.com/ponyo877/scrim/pkg/gameservers"
	"github.com/ponyo877/scrim/pkg/podspec"
	sidecarv1 "github.com/ponyo877/scrim/proto/sidecar/v1"
)

// This is a model test of the agent against a Docker that remembers.
//
// rapid draws a history for one node. GameServers are placed and deleted,
// game servers call the SDK and exit, containers vanish, Docker fails,
// reports get lost, the agent's copy of its GameServers falls behind, and
// the agent itself dies in the middle of what it is doing and is started
// again. Between those the agent makes its passes.
//
// Two promises are held against every step: the agent never takes down a
// running container of a GameServer that is doing fine, and it never says
// CreateFailed of a GameServer that Docker did not fail. A third is held
// against the end of every quiet spell: a few passes without faults bring
// up the containers of every GameServer and clear away those of none.

func TestTheAgentKeepsItsPromisesThroughCrashesAndFaults(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		n := newModelNode(t)
		t.Repeat(map[string]func(*rapid.T){
			"a GameServer is placed":              n.place,
			"a GameServer is deleted":             n.delete,
			"a deleted GameServer is forgotten":   n.forget,
			"a game server calls the SDK":         n.callSDK,
			"a game server exits":                 n.exit,
			"containers vanish":                   n.vanish,
			"the agent makes a pass":              n.pass,
			"the agent will die at a Docker call": n.armCrash,
			"a Docker call will fail":             n.armFailure,
			"a report will get lost":              n.armLoss,
			"the agent's copy falls behind":       n.freeze,
			"the agent's copy catches up":         n.thaw,
			"a quiet spell":                       n.quiet,
			"":                                    n.promisesKept,
		})
	})
}

// modelNode is a node: a Docker, the part of the control plane an agent
// talks to, and the agent between them.
type modelNode struct {
	t      *rapid.T
	docker *memoryDocker
	cp     *placements
	agent  *Agent
	ctx    context.Context
	die    context.CancelFunc
}

func newModelNode(t *rapid.T) *modelNode {
	n := &modelNode{t: t, cp: &placements{servers: map[string]*agonesv1.GameServer{}, failed: map[string]bool{}}}
	n.docker = &memoryDocker{standing: n.cp.standing, failed: n.cp.failed}
	n.startAgent()
	t.Cleanup(func() { n.die() })
	return n
}

// startAgent starts an agent that remembers nothing, as after a restart.
func (n *modelNode) startAgent() {
	n.ctx, n.die = context.WithCancel(context.Background())
	n.docker.crash = n.die
	n.agent = New(Config{NodeName: "node-1", SidecarImage: "sidecar:dev", SidecarDial: "host.docker.internal:9447"}, n.cp, n.docker)
}

// ---- What happens to the node.

func aName() *rapid.Generator[string] {
	return rapid.SampledFrom([]string{"gs-1", "gs-2", "gs-3"})
}

func (n *modelNode) place(t *rapid.T) {
	name := aName().Draw(t, "name")
	if !n.cp.place(name) {
		t.Skip("it is there already")
	}
}

func (n *modelNode) delete(t *rapid.T) {
	n.cp.change(aName().Draw(t, "name"), func(gs *agonesv1.GameServer) {
		if gs.DeletionTimestamp == nil {
			gs.DeletionTimestamp = &metav1.Time{Time: time.Unix(0, 0)}
		}
	})
}

// forget is what the control plane does with a deleted GameServer whose
// node it cannot reach: it drops the record without waiting for the agent.
func (n *modelNode) forget(t *rapid.T) {
	n.cp.forget(aName().Draw(t, "name"))
}

func (n *modelNode) callSDK(t *rapid.T) {
	name := aName().Draw(t, "name")
	if !n.docker.runs(name) {
		t.Skip("no game server runs there")
	}
	event := rapid.SampledFrom([]gameservers.Event{
		gameservers.SDKReady, gameservers.SDKAllocate, gameservers.SDKReserve, gameservers.SDKShutdown,
	}).Draw(t, "event")
	// The sidecar speaks for the GameServer its containers were made for.
	_ = n.cp.Report(context.Background(), gameservers.Report{Namespace: "default", Name: name, UID: n.docker.madeFor(name), Event: event})
}

func (n *modelNode) exit(t *rapid.T) {
	n.docker.exit(aName().Draw(t, "name"), rapid.IntRange(0, 1).Draw(t, "code"))
}

func (n *modelNode) vanish(t *rapid.T) {
	n.docker.vanish(aName().Draw(t, "name"))
}

func aDockerCall(t *rapid.T) trap {
	return trap{
		call: rapid.SampledFrom([]string{"create", "start", "stop", "remove"}).Draw(t, "call"),
		name: aName().Draw(t, "name"),
	}
}

func (n *modelNode) armCrash(t *rapid.T) {
	at := aDockerCall(t)
	at.crash = true
	n.docker.arm(at)
}

func (n *modelNode) armFailure(t *rapid.T) {
	n.docker.arm(aDockerCall(t))
}

func (n *modelNode) armLoss(t *rapid.T) {
	n.cp.lose(aName().Draw(t, "name"), rapid.SampledFrom([]gameservers.Event{
		gameservers.ContainersCreated, gameservers.ContainersRunning, gameservers.ContainersRemoved, gameservers.CreateFailed,
	}).Draw(t, "event"))
}

// freeze is an agent on another machine whose stream of GameServers has
// stopped arriving for a while. Its reports still get through.
func (n *modelNode) freeze(*rapid.T) { n.cp.freeze() }

func (n *modelNode) thaw(*rapid.T) { n.cp.thaw() }

// pass is one pass of the agent, with everything it starts carried to the
// end. An agent that died on the way is started again, as systemd does.
func (n *modelNode) pass(*rapid.T) {
	n.agent.reconcile(n.ctx)
	deadline := time.Now().Add(10 * time.Second)
	for len(n.agent.inProgress()) > 0 {
		require.True(n.t, time.Now().Before(deadline), "an action never ends")
		time.Sleep(50 * time.Microsecond)
	}
	if n.ctx.Err() != nil {
		n.startAgent()
	}
}

// ---- The promises.

func (n *modelNode) promisesKept(*rapid.T) {
	require.Empty(n.t, n.docker.complaints(), "what Docker saw")
	require.Empty(n.t, n.cp.complaints(), "what the control plane heard")
}

// quiet is a spell without faults, long enough for the agent to finish
// whatever it has to do. Then everything placed here runs and nothing else
// does.
func (n *modelNode) quiet(t *rapid.T) {
	n.docker.disarm()
	n.cp.thaw()
	n.cp.lose("", "")
	before := ""
	for round := 0; ; round++ {
		require.Less(t, round, 30, "the agent never comes to rest")
		n.pass(t)
		now := n.docker.picture() + n.cp.picture()
		if now == before {
			break
		}
		before = now
	}
	n.promisesKept(t)
	for _, gs := range n.cp.snapshot() {
		n.hasWhatItNeeds(gs)
	}
	for _, c := range n.docker.list() {
		gs := n.cp.get(c.Labels[podspec.LabelGameServer])
		require.NotNil(n.t, gs, "container %s belongs to no GameServer", c.Name)
		require.Equal(n.t, string(gs.UID), c.Labels[podspec.LabelUID], "container %s belongs to an earlier GameServer of that name", c.Name)
	}
}

func (n *modelNode) hasWhatItNeeds(gs *agonesv1.GameServer) {
	require.Nil(n.t, gs.DeletionTimestamp, "%s is still being deleted", gs.Name)
	if gameservers.Final(gs.Status.State) {
		return
	}
	require.NotContains(n.t, []agonesv1.GameServerState{agonesv1.GameServerStateCreating, agonesv1.GameServerStateStarting},
		gs.Status.State, "%s never came up", gs.Name)
	require.True(n.t, n.docker.runs(gs.Name), "%s is %s without its containers running", gs.Name, gs.Status.State)
}

// ---- The control plane's side.

// placements is what the control plane has placed on the node, and what it
// does about the agent's reports: the state machine, and the two things
// the rest of the control plane does at once. A request for Ready is
// granted, and a GameServer that shut down is deleted.
type placements struct {
	mu         sync.Mutex
	servers    map[string]*agonesv1.GameServer
	made       int                    // for the UIDs
	frozen     []*agonesv1.GameServer // what a lagging agent still sees
	behind     bool
	lost       report          // a report that will not get through, once
	failed     map[string]bool // GameServers that Docker did fail
	complained []string
}

type report struct {
	name  string
	event gameservers.Event
}

func (p *placements) place(name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.servers[name] != nil {
		return false
	}
	p.made++
	gs := &agonesv1.GameServer{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name, UID: types.UID(fmt.Sprint("uid-", p.made))}}
	gs.Spec.Container = "game"
	gs.Spec.Template.Spec.Containers = []corev1.Container{{Name: "game", Image: "example/game:1"}}
	gs.Status.State = agonesv1.GameServerStateCreating
	p.servers[name] = gs
	delete(p.failed, name)
	return true
}

func (p *placements) change(name string, change func(*agonesv1.GameServer)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if gs := p.servers[name]; gs != nil {
		change(gs)
	}
}

func (p *placements) forget(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if gs := p.servers[name]; gs != nil && gs.DeletionTimestamp != nil {
		delete(p.servers, name)
	}
}

func (p *placements) get(name string) *agonesv1.GameServer {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.servers[name].DeepCopy()
}

func (p *placements) lose(name string, event gameservers.Event) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lost = report{name, event}
}

func (p *placements) freeze() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.behind {
		p.frozen, p.behind = p.copies(), true
	}
}

func (p *placements) thaw() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.frozen, p.behind = nil, false
}

func (p *placements) snapshot() []*agonesv1.GameServer {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.copies()
}

// copies is called with the lock held.
func (p *placements) copies() []*agonesv1.GameServer {
	out := make([]*agonesv1.GameServer, 0, len(p.servers))
	for _, gs := range p.servers {
		out = append(out, gs.DeepCopy())
	}
	slices.SortFunc(out, func(a, b *agonesv1.GameServer) int { return strings.Compare(a.Name, b.Name) })
	return out
}

func (p *placements) picture() string {
	var b strings.Builder
	for _, gs := range p.snapshot() {
		fmt.Fprintf(&b, "%s=%s,%v ", gs.Name, gs.Status.State, gs.DeletionTimestamp != nil)
	}
	return b.String()
}

func (p *placements) complaints() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.complained)
}

// standing reports whether the GameServer a container belongs to is doing
// fine: it is the one placed here under that name, nobody asked for it to
// go, and its containers were taken note of.
func (p *placements) standing(c docker.Container) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	gs := p.servers[c.Labels[podspec.LabelGameServer]]
	if gs == nil || string(gs.UID) != c.Labels[podspec.LabelUID] || gs.DeletionTimestamp != nil {
		return false
	}
	return gs.Status.State != agonesv1.GameServerStateCreating && !gameservers.Final(gs.Status.State)
}

func (p *placements) Register(context.Context, *scrimv1.Node) error { return nil }

func (p *placements) Assigned(string) ([]*agonesv1.GameServer, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.behind {
		return p.frozen, true
	}
	return p.copies(), true
}

func (p *placements) Changes() (<-chan struct{}, func()) { return nil, func() {} }

func (p *placements) SetMetadata(context.Context, string, string, map[string]string, map[string]string) error {
	return nil
}

func (p *placements) UpdateCounts(context.Context, string, string, string, []*sidecarv1.CounterUpdate, []*sidecarv1.ListUpdate) (*agonesv1.GameServer, error) {
	return nil, nil
}

func (p *placements) Report(_ context.Context, r gameservers.Report) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.lost == (report{r.Name, r.Event}) {
		p.lost = report{}
		return errors.New("the report did not get through")
	}
	if gs := p.servers[r.Name]; gs != nil && (r.UID == "" || r.UID == gs.UID) {
		p.apply(gs, r)
	}
	return nil
}

// apply does what gameservers.Controller does with a report. This is where
// the second promise is watched over. Called with the lock held.
func (p *placements) apply(gs *agonesv1.GameServer, r gameservers.Report) {
	deleting := gs.DeletionTimestamp != nil
	if r.Event == gameservers.ContainersRemoved && deleting {
		delete(p.servers, r.Name)
	}
	next, applies := gameservers.Next(gs.Status.State, r.Event)
	if !applies || deleting {
		return
	}
	if r.Event == gameservers.CreateFailed && !p.failed[r.Name] {
		p.complained = append(p.complained, fmt.Sprintf("CreateFailed for %s (%s), which Docker did not fail: %s", r.Name, gs.Status.State, r.Detail))
	}
	gs.Status.State = next
	switch next {
	case agonesv1.GameServerStateRequestReady:
		gs.Status.State = agonesv1.GameServerStateReady
	case agonesv1.GameServerStateShutdown:
		gs.DeletionTimestamp = &metav1.Time{Time: time.Unix(0, 0)}
	}
}

// ---- Docker's side.

// trap is something that goes wrong at the next Docker call of one kind for
// one GameServer: the call fails, or the agent dies at it.
type trap struct {
	call  string
	name  string
	crash bool
}

// memoryDocker is a Docker that keeps its containers in memory and behaves
// like the real one where the agent depends on it: names are unique, the
// newest container is listed first, and a container cannot join the network
// of one that is not running.
type memoryDocker struct {
	mu         sync.Mutex
	containers []*kept
	trap       trap
	crash      func()                      // ends the agent
	standing   func(docker.Container) bool // whether a container's GameServer is doing fine
	failed     map[string]bool             // GameServers that a create or a start failed for
	complained []string
}

type kept struct {
	docker.Container
	network string // the name of the container whose network it shares
}

func (d *memoryDocker) arm(t trap) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.trap = t
}

func (d *memoryDocker) disarm() { d.arm(trap{}) }

// sprung reports what the trap does to a call, if the call is the one it
// waits for. Called with the lock held.
func (d *memoryDocker) sprung(ctx context.Context, call string, c docker.Container) error {
	if err := ctx.Err(); err != nil {
		return err // the agent is dead already
	}
	name := c.Labels[podspec.LabelGameServer]
	if d.trap.call != call || d.trap.name != name {
		return nil
	}
	crash := d.trap.crash
	d.trap = trap{}
	if crash {
		d.crash()
		return context.Canceled
	}
	if call == "create" || call == "start" {
		d.failed[name] = true
	}
	return fmt.Errorf("docker could not %s a container of %s", call, name)
}

func (d *memoryDocker) List(ctx context.Context, _ string) ([]docker.Container, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return d.list(), nil
}

// list returns the containers, the newest first.
func (d *memoryDocker) list() []docker.Container {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]docker.Container, 0, len(d.containers))
	for _, c := range slices.Backward(d.containers) {
		out = append(out, c.Container)
	}
	return out
}

func (d *memoryDocker) HasImage(context.Context, string) (bool, error) { return true, nil }

func (d *memoryDocker) Pull(context.Context, string) error { return nil }

func (d *memoryDocker) Create(ctx context.Context, spec docker.Spec) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	c := docker.Container{ID: spec.Name, Name: spec.Name, Labels: spec.Labels, State: "created"}
	if err := d.sprung(ctx, "create", c); err != nil {
		return "", err
	}
	if d.find(spec.Name) != nil {
		d.complained = append(d.complained, "a second container was asked for under the name "+spec.Name)
		return "", &docker.Error{Status: http.StatusConflict, Message: "Conflict. The container name is already in use"}
	}
	d.containers = append(d.containers, &kept{Container: c, network: spec.ShareNetworkWith})
	return c.ID, nil
}

// find is called with the lock held.
func (d *memoryDocker) find(id string) *kept {
	for _, c := range d.containers {
		if c.ID == id {
			return c
		}
	}
	return nil
}

func (d *memoryDocker) Start(ctx context.Context, id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	c := d.find(id)
	if c == nil {
		return &docker.Error{Status: http.StatusNotFound, Message: "No such container: " + id}
	}
	if err := d.sprung(ctx, "start", c.Container); err != nil {
		return err
	}
	if owner := d.find(c.network); c.network != "" && (owner == nil || owner.State != "running") {
		d.failed[c.Labels[podspec.LabelGameServer]] = true
		return fmt.Errorf("cannot join network of a non running container: %s", c.network)
	}
	c.State = "running"
	return nil
}

func (d *memoryDocker) Stop(ctx context.Context, id string, _ time.Duration) error {
	return d.takeDown(ctx, "stop", id, func(c *kept) { c.State, c.ExitCode = "exited", 137 })
}

func (d *memoryDocker) Remove(ctx context.Context, id string) error {
	return d.takeDown(ctx, "remove", id, func(c *kept) {
		d.containers = slices.DeleteFunc(d.containers, func(other *kept) bool { return other == c })
	})
}

// takeDown stops or removes a container. This is where the first promise
// is watched over.
func (d *memoryDocker) takeDown(ctx context.Context, call, id string, do func(*kept)) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	c := d.find(id)
	if c == nil {
		return nil // gone already, which is what was wanted
	}
	if err := d.sprung(ctx, call, c.Container); err != nil {
		return err
	}
	if c.State == "running" && d.standing(c.Container) {
		d.complained = append(d.complained, fmt.Sprintf("%s of %s, which runs and whose GameServer is doing fine", call, c.Name))
	}
	do(c)
	return nil
}

func (d *memoryDocker) RemoveVolumes(context.Context, string) error { return nil }

func (d *memoryDocker) Logs(context.Context, string, bool, int) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}

func (d *memoryDocker) complaints() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.complained)
}

func (d *memoryDocker) picture() string {
	var b strings.Builder
	for _, c := range d.list() {
		fmt.Fprintf(&b, "%s=%s ", c.Name, c.State)
	}
	return b.String()
}

// of returns the containers of the GameServer called name. Called with the
// lock held.
func (d *memoryDocker) of(name string) []*kept {
	var out []*kept
	for _, c := range d.containers {
		if c.Labels[podspec.LabelGameServer] == name {
			out = append(out, c)
		}
	}
	return out
}

// runs reports whether the GameServer has its sidecar and its game running.
func (d *memoryDocker) runs(name string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	running := 0
	for _, c := range d.of(name) {
		if c.State == "running" {
			running++
		}
	}
	return running == 2
}

// madeFor returns the uid of the GameServer the containers were made for.
func (d *memoryDocker) madeFor(name string) types.UID {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, c := range d.of(name) {
		return types.UID(c.Labels[podspec.LabelUID])
	}
	return ""
}

// exit is the game server's process ending by itself.
func (d *memoryDocker) exit(name string, code int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, c := range d.of(name) {
		if c.Labels[podspec.LabelRole] == podspec.RoleGame && c.State == "running" {
			c.State, c.ExitCode = "exited", code
		}
	}
}

// vanish is somebody removing a GameServer's containers by hand, or a
// machine that came back without them.
func (d *memoryDocker) vanish(name string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	gone := d.of(name)
	d.containers = slices.DeleteFunc(d.containers, func(c *kept) bool { return slices.Contains(gone, c) })
}
