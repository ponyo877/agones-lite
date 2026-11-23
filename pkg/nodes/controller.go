package nodes

import (
	"context"
	"time"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	litev1 "github.com/ponyo877/agones-lite/pkg/apis/lite/v1"
	"github.com/ponyo877/agones-lite/pkg/control"
	"github.com/ponyo877/agones-lite/pkg/events"
	"github.com/ponyo877/agones-lite/pkg/gameservers"
	"github.com/ponyo877/agones-lite/pkg/store"
)

// Kind is the name Nodes go by in manifests and in the event history.
const Kind = "Node"

// resyncPeriod is short because the grace periods are judged by the clock,
// not by an event.
const resyncPeriod = 10 * time.Second

// warmUp is how long a control plane that has just started gives nothing
// up, so that the agents get to dial again first. See StartOver.
const warmUp = 30 * time.Second

// Graces is how long a disconnected node's GameServers are waited for.
//
// A lost connection may mean a dead machine or only a broken network, and
// from here the two look the same. Servers without a session are cheap to
// replace, so they are given up quickly. A session may still be running fine
// with its players, so an Allocated server is waited for much longer.
type Graces struct {
	Unallocated time.Duration
	Allocated   time.Duration
}

// DefaultGraces are the waiting times used unless configured otherwise.
var DefaultGraces = Graces{Unallocated: 5 * time.Minute, Allocated: 24 * time.Hour}

// GivenUp returns the GameServers on node that have been out of reach for
// too long. Reserved counts as "without a session".
func GivenUp(node *litev1.Node, onNode []*agonesv1.GameServer, graces Graces, now time.Time) []*agonesv1.GameServer {
	if node.Status.Phase != litev1.NodeDisconnected {
		return nil
	}
	away := now.Sub(node.Status.LastHeartbeatTime.Time)
	var lost []*agonesv1.GameServer
	for _, gs := range onNode {
		limit := graces.Unallocated
		if gs.Status.State == agonesv1.GameServerStateAllocated {
			limit = graces.Allocated
		}
		if away >= limit && !gameservers.Final(gs.Status.State) {
			lost = append(lost, gs)
		}
	}
	return lost
}

// Drained returns the GameServers a draining node should now shut down:
// those without a session. Allocated and Reserved ones are left to finish.
func Drained(node *litev1.Node, onNode []*agonesv1.GameServer) []*agonesv1.GameServer {
	if !node.Spec.Drain {
		return nil
	}
	var idle []*agonesv1.GameServer
	for _, gs := range onNode {
		if _, ok := gameservers.Next(gs.Status.State, gameservers.ScaledDown); ok && gs.DeletionTimestamp == nil {
			idle = append(idle, gs)
		}
	}
	return idle
}

// Controller carries out GivenUp and Drained for every node.
type Controller struct {
	nodes       store.Typed[*litev1.Node]
	servers     store.Typed[*agonesv1.GameServer]
	gameServers *gameservers.Controller
	events      *events.Recorder
	graces      Graces
	now         func() time.Time
	started     time.Time // when StartOver ran; the warm-up counts from here
}

// NewController returns a Controller working on s.
func NewController(s *store.Store, gameServers *gameservers.Controller, recorder *events.Recorder, graces Graces) *Controller {
	return &Controller{
		nodes:       store.NodesOf(s),
		servers:     store.GameServersOf(s),
		gameServers: gameServers,
		events:      recorder,
		graces:      graces,
		now:         time.Now,
	}
}

// StartOver marks every node Disconnected. A control plane that has just
// started has not heard from any agent yet, whatever the store remembers:
// it may have been down for a while, and a machine may have died meanwhile.
// Agents that are still there say so within seconds. The others are waited
// for like any node that went quiet. For a node recorded as Ready the wait
// begins now. One that was Disconnected already keeps the time it was last
// heard from: were it to begin again with every start, a control plane
// restarting more often than the grace lasts would never give a node up.
//
// That time may lie further back than the grace, when the control plane
// was down for long. Its agent may be there all the same and only needs a
// moment to dial again, so for warmUp nothing is given up: see givenUp.
func (c *Controller) StartOver() error {
	c.started = c.now()
	for _, node := range c.nodes.List("") {
		if node.Status.Phase == litev1.NodeDisconnected {
			continue
		}
		node.Status.Phase = litev1.NodeDisconnected
		node.Status.LastHeartbeatTime = metav1.NewTime(c.now())
		if _, err := c.nodes.Update(node); err != nil {
			return err
		}
	}
	return nil
}

// Run watches over the nodes until ctx is done.
func (c *Controller) Run(ctx context.Context) {
	loop := control.Loop{Sync: c.sync, All: c.all, Resync: resyncPeriod}
	loop.Run(ctx,
		control.Watch{Subscribe: c.nodes.Watch, Keys: control.Self},
		control.Watch{Subscribe: c.servers.Watch, Keys: nodeOf},
	)
}

// all names every node that exists or that a GameServer still points to.
// The second group matters after a node was deleted.
func (c *Controller) all() []store.Key {
	names := map[string]bool{}
	for _, node := range c.nodes.View("") {
		names[node.Name] = true
	}
	for _, gs := range c.servers.View("") {
		if gs.Status.NodeName != "" {
			names[gs.Status.NodeName] = true
		}
	}
	keys := make([]store.Key, 0, len(names))
	for name := range names {
		keys = append(keys, store.Key{Kind: store.Nodes, Name: name})
	}
	return keys
}

// nodeOf names the node a changed GameServer is on.
func nodeOf(e store.Event) []store.Key {
	gs, ok := e.Object.(*agonesv1.GameServer)
	if !ok || gs.Status.NodeName == "" {
		return nil
	}
	return []store.Key{{Kind: store.Nodes, Name: gs.Status.NodeName}}
}

func (c *Controller) sync(_ context.Context, key store.Key) error {
	onNode := c.onNode(key.Name)
	node, exists := c.nodes.Get("", key.Name)
	if !exists {
		// The operator deleted the node: nothing on it will be heard of again.
		return c.report(key.Name, onNode, gameservers.NodeLost, "its node was deleted")
	}
	if err := c.report(key.Name, c.givenUp(node, onNode), gameservers.NodeLost, "its node has been out of reach for too long"); err != nil {
		return err
	}
	return c.report(key.Name, Drained(node, onNode), gameservers.ScaledDown, "its node is being drained")
}

// givenUp is GivenUp, except during the warm-up after a start. The warm-up
// is never longer than the shorter grace: whoever sets that to seconds
// does not want to wait half a minute.
func (c *Controller) givenUp(node *litev1.Node, onNode []*agonesv1.GameServer) []*agonesv1.GameServer {
	now := c.now()
	if now.Sub(c.started) < min(warmUp, c.graces.Unallocated) {
		return nil
	}
	return GivenUp(node, onNode, c.graces, now)
}

func (c *Controller) onNode(name string) []*agonesv1.GameServer {
	var out []*agonesv1.GameServer
	for _, gs := range c.servers.View("") {
		if gs.Status.NodeName == name {
			out = append(out, gs)
		}
	}
	return out
}

// report applies one Event to each of the GameServers and notes why.
func (c *Controller) report(node string, servers []*agonesv1.GameServer, event gameservers.Event, why string) error {
	for _, gs := range servers {
		if gameservers.Final(gs.Status.State) {
			continue
		}
		if err := c.gameServers.Report(gameservers.Report{Namespace: gs.Namespace, Name: gs.Name, Event: event}); err != nil {
			return err
		}
		c.events.Record(Kind, "", node, events.Warning, string(event), "%s/%s: %s", gs.Namespace, gs.Name, why)
	}
	return nil
}

// Cordon stops new game servers and allocations from landing on a node.
// With drain, the node also sheds the game servers that have no session.
func (c *Controller) Cordon(name string, drain bool) error {
	return c.nodes.Change("", name, func(node *litev1.Node) bool {
		node.Spec.Unschedulable, node.Spec.Drain = true, drain
		return true
	})
}

// Uncordon makes a node available again.
func (c *Controller) Uncordon(name string) error {
	return c.nodes.Change("", name, func(node *litev1.Node) bool {
		node.Spec.Unschedulable, node.Spec.Drain = false, false
		return true
	})
}
