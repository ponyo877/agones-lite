package gameservers

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	litev1 "github.com/ponyo877/agones-lite/pkg/apis/lite/v1"
	"github.com/ponyo877/agones-lite/pkg/control"
	"github.com/ponyo877/agones-lite/pkg/events"
	"github.com/ponyo877/agones-lite/pkg/scheduling"
	"github.com/ponyo877/agones-lite/pkg/store"
)

const (
	// resyncPeriod is how often every GameServer is looked at again, in case
	// a change notification was missed.
	resyncPeriod = 30 * time.Second
	// Kind is the name GameServers go by in manifests and in the event history.
	Kind = "GameServer"
	// LastAllocatedAnnotation records when a GameServer was last allocated.
	// Same key as upstream's gameserverallocations.LastAllocatedAnnotationKey.
	LastAllocatedAnnotation = "agones.dev/last-allocated"
	// LastCountsChangeAnnotation names the last change to Counters and Lists
	// made through the SDK. The sidecar sends a change again when the answer
	// got lost; the name keeps it from being made twice.
	LastCountsChangeAnnotation = "agones-lite/last-counts-change"
)

// Report is an Event that an agent or a sidecar saw happen to a GameServer.
type Report struct {
	Namespace string
	Name      string
	// UID, when set, says which GameServer of that name is meant. One made
	// later under the same name is another GameServer: what an agent saw
	// happen to the first one's containers says nothing about it.
	UID   types.UID
	Event Event
	// Reserve is how long an SDKReserve lasts. Zero means until told otherwise.
	Reserve time.Duration
	// Detail is what the reporter knows of the cause, for the event history:
	// for CreateFailed, what Docker said. It may be empty.
	Detail string
}

// about reports whether gs is the GameServer the Report speaks of.
func (r Report) about(gs *agonesv1.GameServer) bool {
	return r.UID == "" || r.UID == gs.UID
}

// maxDetail is how much of a Report's detail the event history keeps.
const maxDetail = 300

// message is the line the event history gets for a move out of from.
func (r Report) message(from agonesv1.GameServerState) string {
	message := fmt.Sprintf("%s: was %s", r.Event, from)
	if detail := brief(r.Detail); detail != "" {
		message += ": " + detail
	}
	return message
}

// brief makes a detail fit one line of the event history. It comes from
// another machine, which had it from Docker, so it may be long and have line
// breaks. Of a long one the two ends are kept: an error names what was tried
// first and the cause last.
func brief(detail string) string {
	line := []rune(strings.Join(strings.Fields(detail), " "))
	if len(line) <= maxDetail {
		return string(line)
	}
	half := maxDetail / 2
	return string(line[:half]) + " ... " + string(line[len(line)-half:])
}

// Controller owns the state of every GameServer.
type Controller struct {
	servers store.Typed[*agonesv1.GameServer]
	nodes   store.Typed[*litev1.Node]
	events  *events.Recorder
	now     func() time.Time
	// Entered, when set, is told about every state a GameServer enters.
	// It is how the metrics learn of transitions.
	Entered func(gs *agonesv1.GameServer, state agonesv1.GameServerState)
	// waiting holds the GameServers that found no node, so that the reason
	// is recorded once and not on every pass. Only the sync loop touches it.
	waiting map[store.Key]bool
	// heard holds every node as it was when last heard of, to tell a change
	// from a sign of life. Only what follows the nodes touches it.
	heard map[string]*litev1.Node
}

// thoseWaiting is the key under which the GameServers without a node are
// looked at: all of them in one pass, see placeWaiting. No GameServer has
// this key, because none is without a name.
var thoseWaiting = store.Key{Kind: store.GameServers}

// NewController returns a Controller working on s. History goes to recorder,
// which may be nil.
func NewController(s *store.Store, recorder *events.Recorder) *Controller {
	return &Controller{
		servers: store.GameServersOf(s),
		nodes:   store.NodesOf(s),
		events:  recorder,
		now:     s.Now,
		waiting: map[store.Key]bool{},
		heard:   map[string]*litev1.Node{},
	}
}

// Loop says what the controller looks at, and when. Run runs it; a test may
// step through it with a control.Stepper instead.
func (c *Controller) Loop() (control.Loop, []control.Watch) {
	return control.Loop{Sync: c.sync, All: c.all, Resync: resyncPeriod}, []control.Watch{
		{Subscribe: c.servers.Watch, Keys: afterServerChange},
		{Subscribe: c.nodes.Watch, Keys: c.afterNodeChange},
	}
}

// Run keeps GameServers moving until ctx is done.
func (c *Controller) Run(ctx context.Context) {
	control.Run(ctx, c)
}

// afterServerChange names what to look at after a GameServer changed:
// itself, and those that wait for a node when it is one of them. One that
// is gone has left room on its node, which any of them may use.
func afterServerChange(e store.Event) []store.Key {
	switch {
	case e.Type == store.Deleted:
		return []store.Key{thoseWaiting}
	case e.Object.(*agonesv1.GameServer).Status.NodeName == "":
		return []store.Key{e.Key, thoseWaiting}
	}
	return control.Self(e)
}

// afterNodeChange names what to look at after a node changed: those that
// wait for a node, which may have room now, and the GameServers on it that
// are being deleted, which are dropped when its agent cannot be reached.
//
// Every node says that it is still there every few seconds, and that is a
// change to its record too. It concerns no GameServer. Looking at all of
// them each time would be work in proportion to nodes times GameServers,
// for ever, with nothing going on.
func (c *Controller) afterNodeChange(e store.Event) []store.Key {
	node := e.Object.(*litev1.Node)
	was := c.heard[node.Name]
	c.heard[node.Name] = node
	if e.Type == store.Deleted {
		delete(c.heard, node.Name)
	} else if onlyASignOfLife(was, node) {
		return nil
	}
	keys := []store.Key{thoseWaiting}
	for _, gs := range c.servers.View("") {
		if gs.Status.NodeName == node.Name && gs.DeletionTimestamp != nil {
			keys = append(keys, store.KeyOf(store.GameServers, gs))
		}
	}
	return keys
}

// onlyASignOfLife reports whether a node differs from what it was in
// nothing but the time it was last heard from.
func onlyASignOfLife(was, node *litev1.Node) bool {
	if was == nil {
		return false
	}
	before, after := was.Status, node.Status
	before.LastHeartbeatTime, after.LastHeartbeatTime = metav1.Time{}, metav1.Time{}
	return apiequality.Semantic.DeepEqual(was.Spec, node.Spec) && apiequality.Semantic.DeepEqual(before, after)
}

func (c *Controller) all() []store.Key {
	keys := []store.Key{thoseWaiting}
	for _, gs := range c.servers.View("") {
		keys = append(keys, store.KeyOf(store.GameServers, gs))
	}
	return keys
}

// step is what the sync loop should do for one GameServer.
type step int

const (
	wait        step = iota // nothing to do until something is reported
	place                   // choose a node and host ports
	grantReady              // RequestReady becomes Ready
	startDelete             // Shutdown: ask for the containers to be removed
	forget                  // nothing left to clean up: drop the record
)

// decide is the judgement half of the sync loop. It looks only at gs and at
// whether the agent of its node can currently be reached.
func decide(gs *agonesv1.GameServer, reachable bool) step {
	unplaced := gs.Status.NodeName == ""
	switch {
	case gs.DeletionTimestamp != nil && (unplaced || !reachable):
		// Nobody will report the cleanup. If the node comes back, its agent
		// finds containers that belong to nothing and removes them.
		return forget
	case gs.DeletionTimestamp != nil:
		return wait // the agent reports ContainersRemoved when it is done
	case gs.Status.State == agonesv1.GameServerStateShutdown:
		return startDelete
	case unplaced && !Final(gs.Status.State):
		return place
	case gs.Status.State == agonesv1.GameServerStateRequestReady:
		return grantReady
	}
	return wait
}

// sync is the execution half: it carries out what decide chose. A node is
// not chosen here, for one GameServer: placeWaiting does that for all that
// wait, under a key of its own.
func (c *Controller) sync(_ context.Context, key store.Key) error {
	if key == thoseWaiting {
		return c.placeWaiting()
	}
	gs, ok := c.servers.Peek(key.Namespace, key.Name) // only to decide: most of the time there is nothing to do
	if !ok {
		return nil
	}
	switch decide(gs, c.reachable(gs.Status.NodeName)) {
	case grantReady:
		return c.Report(Report{Namespace: gs.Namespace, Name: gs.Name, Event: Readied})
	case startDelete:
		return c.Delete(gs.Namespace, gs.Name)
	case forget:
		return ignoreNotFound(c.servers.Delete(gs.Namespace, gs.Name))
	}
	return nil
}

// reachable reports whether the node's agent is connected.
func (c *Controller) reachable(nodeName string) bool {
	node, ok := c.nodes.Peek("", nodeName)
	return ok && node.Status.Phase == litev1.NodeReady
}

// placeWaiting chooses a node and host ports for every GameServer that has
// none, the one that has waited longest first.
//
// It is one pass for all of them because the choice needs to know what
// every node holds, which takes a look at every GameServer. Looking once
// per GameServer, a Fleet of ten thousand would look a hundred million
// times.
func (c *Controller) placeWaiting() error {
	servers := c.servers.View("")
	waiting := waitingForANode(servers)
	if len(waiting) == 0 {
		clear(c.waiting)
		return nil
	}
	room := scheduling.RoomOn(c.nodes.View(""), servers)
	noRoom := map[store.Key]bool{}
	var errs error
	for _, gs := range waiting {
		if placed, found := room.Place(gs); found {
			errs = errors.Join(errs, c.settle(placed))
		} else {
			noRoom[store.KeyOf(store.GameServers, gs)] = true
		}
	}
	c.sayWhoWaits(noRoom)
	return errs
}

// waitingForANode returns the GameServers that are to be placed, by how
// long they have waited, and by name where that does not tell them apart.
func waitingForANode(servers []*agonesv1.GameServer) []*agonesv1.GameServer {
	var waiting []*agonesv1.GameServer
	for _, gs := range servers {
		if gs.Status.NodeName == "" && decide(gs, false) == place {
			waiting = append(waiting, gs)
		}
	}
	slices.SortFunc(waiting, func(a, b *agonesv1.GameServer) int {
		return cmp.Or(a.CreationTimestamp.Compare(b.CreationTimestamp.Time), cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Name, b.Name))
	})
	return waiting
}

// settle stores a GameServer that was given its node.
func (c *Controller) settle(placed *agonesv1.GameServer) error {
	placed.Status.State, _ = Next(placed.Status.State, Placed)
	if _, err := c.servers.Update(placed); err != nil {
		return err
	}
	c.events.Record(Kind, placed.Namespace, placed.Name, events.Normal, "Placed", "on node %s", placed.Status.NodeName)
	return nil
}

// sayWhoWaits records, for each GameServer that found no node, why it is
// waiting: once, and not again on every pass that finds the same.
func (c *Controller) sayWhoWaits(noRoom map[store.Key]bool) {
	for key := range noRoom {
		if !c.waiting[key] {
			c.events.Record(Kind, key.Namespace, key.Name, events.Warning, "Unschedulable", "no node has room; waiting for one")
		}
	}
	c.waiting = noRoom
}

// Delete asks for a GameServer to be removed. The record stays until its
// containers are gone, which the agent reports with ContainersRemoved.
func (c *Controller) Delete(namespace, name string) error {
	return c.servers.MarkDeleted(namespace, name, c.now())
}

// Report applies an Event to a GameServer. Events that do not apply in the
// GameServer's current state are ignored.
func (c *Controller) Report(r Report) error {
	if r.Event == ContainersRemoved {
		return c.forgetDeleted(r)
	}
	var from, to agonesv1.GameServerState
	var moved *agonesv1.GameServer
	err := c.servers.Change(r.Namespace, r.Name, func(gs *agonesv1.GameServer) bool {
		next, ok := Next(gs.Status.State, r.Event)
		if !ok || gs.DeletionTimestamp != nil || !r.about(gs) {
			return false
		}
		node, known := c.nodes.Peek("", gs.Status.NodeName)
		if !known && reachable(next) {
			// Players would be sent to a node nobody knows any more: it was
			// deleted, and the node controller is about to give this
			// GameServer up. Until then it stays what it is.
			return false
		}
		from, to, moved = gs.Status.State, next, gs
		apply(gs, next, r, node, c.now())
		return true
	})
	if err != nil || moved == nil {
		return err
	}
	c.events.Record(Kind, r.Namespace, r.Name, severity(to), string(to), "%s", r.message(from))
	if c.Entered != nil {
		c.Entered(moved, to)
	}
	return nil
}

// severity marks the two states an operator should look into.
func severity(s agonesv1.GameServerState) events.Type {
	if s == agonesv1.GameServerStateUnhealthy || s == agonesv1.GameServerStateError {
		return events.Warning
	}
	return events.Normal
}

// forgetDeleted drops the record of a GameServer whose containers are gone.
func (c *Controller) forgetDeleted(r Report) error {
	gs, ok := c.servers.Peek(r.Namespace, r.Name)
	if !ok || gs.DeletionTimestamp == nil || !r.about(gs) {
		return nil
	}
	return ignoreNotFound(c.servers.Delete(r.Namespace, r.Name))
}

// apply moves gs to next and records what comes with the move.
func apply(gs *agonesv1.GameServer, next agonesv1.GameServerState, r Report, node *litev1.Node, now time.Time) {
	gs.Status.State = next
	gs.Status.ReservedUntil = reservedUntil(next, r.Reserve, now)
	if r.Event == SDKAllocate {
		stamp, _ := now.UTC().MarshalText()
		metav1.SetMetaDataAnnotation(&gs.ObjectMeta, LastAllocatedAnnotation, string(stamp))
	}
	if next == agonesv1.GameServerStateError {
		// A GameServerSet waits a little from this moment before replacing
		// it. Upstream: moveToErrorState in pkg/gameservers/controller.go.
		metav1.SetMetaDataAnnotation(&gs.ObjectMeta, agonesv1.GameServerErroredAtAnnotation, now.UTC().Format(time.RFC3339))
	}
	if reachable(next) {
		publishAddress(gs, node)
	}
}

func reservedUntil(next agonesv1.GameServerState, d time.Duration, now time.Time) *metav1.Time {
	if next != agonesv1.GameServerStateReserved || d <= 0 {
		return nil
	}
	until := metav1.NewTime(now.Add(d))
	return &until
}

// reachable reports whether players can connect in this state, which is when
// the address must be published. Scheduled alone would not do: a game server
// that calls Ready, Reserve or Allocate before its agent reports it running
// never is Scheduled.
//
// Upstream: syncGameServerStartingState, and for Ready the second chance in
// syncGameServerRequestReadyState. Reserve and Allocate have none there.
func reachable(s agonesv1.GameServerState) bool {
	switch s {
	case agonesv1.GameServerStateScheduled, agonesv1.GameServerStateReady,
		agonesv1.GameServerStateReserved, agonesv1.GameServerStateAllocated:
		return true
	}
	return false
}

// publishAddress fills in where players connect.
// Upstream: applyGameServerAddressAndPort in pkg/gameservers/gameservers.go.
func publishAddress(gs *agonesv1.GameServer, node *litev1.Node) {
	gs.Status.Address = node.Status.Address
	gs.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeExternalIP, Address: node.Status.Address}}
	gs.Status.Ports = make([]agonesv1.GameServerStatusPort, len(gs.Spec.Ports))
	for i, p := range gs.Spec.Ports {
		gs.Status.Ports[i] = p.Status()
	}
}

func ignoreNotFound(err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	return err
}

// SDKPrefix goes in front of every label and annotation set through the SDK,
// so a game server cannot overwrite anything else. Same as upstream.
const SDKPrefix = "agones.dev/sdk-"

// UpdateCounts changes Counters and Lists as a game server asked through the
// SDK. It returns the GameServer afterwards, or nil when it is gone. A change
// that arrives a second time, under the same name, is made only once.
func (c *Controller) UpdateCounts(namespace, name, change string, counters []CounterUpdate, lists []ListUpdate) (*agonesv1.GameServer, error) {
	err := c.servers.Change(namespace, name, func(gs *agonesv1.GameServer) bool {
		made := change != "" && gs.Annotations[LastCountsChangeAnnotation] == change
		if made || gs.DeletionTimestamp != nil {
			return false
		}
		ApplyCounts(gs, counters, lists)
		metav1.SetMetaDataAnnotation(&gs.ObjectMeta, LastCountsChangeAnnotation, change)
		return true
	})
	if err != nil {
		return nil, err
	}
	gs, _ := c.servers.Get(namespace, name)
	return gs, nil
}

// SetMetadata adds labels and annotations a game server set through the SDK.
func (c *Controller) SetMetadata(namespace, name string, labels, annotations map[string]string) error {
	return c.servers.Change(namespace, name, func(gs *agonesv1.GameServer) bool {
		if gs.DeletionTimestamp != nil {
			return false
		}
		for key, value := range labels {
			metav1.SetMetaDataLabel(&gs.ObjectMeta, SDKPrefix+key, value)
		}
		for key, value := range annotations {
			metav1.SetMetaDataAnnotation(&gs.ObjectMeta, SDKPrefix+key, value)
		}
		return true
	})
}
