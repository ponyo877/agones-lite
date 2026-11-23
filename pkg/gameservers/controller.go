package gameservers

import (
	"context"
	"errors"
	"log/slog"
	"time"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	litev1 "github.com/ponyo877/agones-lite/pkg/apis/lite/v1"
	"github.com/ponyo877/agones-lite/pkg/queue"
	"github.com/ponyo877/agones-lite/pkg/scheduling"
	"github.com/ponyo877/agones-lite/pkg/store"
)

const (
	// resyncPeriod is how often every GameServer is looked at again, in case
	// a change notification was missed.
	resyncPeriod = 30 * time.Second
	// LastAllocatedAnnotation records when a GameServer was last allocated.
	// Same key as upstream's gameserverallocations.LastAllocatedAnnotationKey.
	LastAllocatedAnnotation = "agones.dev/last-allocated"
	conflictRetries         = 5
)

// Report is an Event that an agent or a sidecar saw happen to a GameServer.
type Report struct {
	Namespace string
	Name      string
	Event     Event
	// Reserve is how long an SDKReserve lasts. Zero means until told otherwise.
	Reserve time.Duration
}

// Controller owns the state of every GameServer.
type Controller struct {
	servers store.Typed[*agonesv1.GameServer]
	nodes   store.Typed[*litev1.Node]
	queue   *queue.Queue[store.Key]
	now     func() time.Time
}

// NewController returns a Controller working on s.
func NewController(s *store.Store) *Controller {
	return &Controller{
		servers: store.GameServersOf(s),
		nodes:   store.NodesOf(s),
		queue:   queue.New[store.Key](),
		now:     time.Now,
	}
}

// Run keeps GameServers moving until ctx is done.
func (c *Controller) Run(ctx context.Context) {
	servers, stopServers := c.servers.Watch()
	defer stopServers()
	nodes, stopNodes := c.nodes.Watch()
	defer stopNodes()
	go c.feed(ctx, servers, nodes)
	c.queue.Run(ctx, c.sync)
}

// feed turns change notifications into queue entries.
func (c *Controller) feed(ctx context.Context, servers, nodes <-chan store.Event) {
	resync := time.NewTicker(resyncPeriod)
	defer resync.Stop()
	c.enqueueAll()
	for {
		select {
		case <-ctx.Done():
			return
		case e := <-servers:
			c.onServerChange(e)
		case <-nodes:
			c.enqueueAll() // a new or changed node may have room for waiting servers
		case <-resync.C:
			c.enqueueAll()
		}
	}
}

func (c *Controller) onServerChange(e store.Event) {
	if e.Type == store.Deleted {
		c.enqueueAll() // its node has room again
		return
	}
	c.queue.Add(e.Key)
}

func (c *Controller) enqueueAll() {
	for _, gs := range c.servers.List("") {
		c.queue.Add(store.KeyOf(store.GameServers, gs))
	}
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

// decide is the judgement half of the sync loop. It looks only at gs.
func decide(gs *agonesv1.GameServer) step {
	unplaced := gs.Status.NodeName == ""
	switch {
	case gs.DeletionTimestamp != nil && unplaced:
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

// sync is the execution half: it carries out what decide chose.
func (c *Controller) sync(_ context.Context, key store.Key) error {
	gs, ok := c.servers.Get(key.Namespace, key.Name)
	if !ok {
		return nil
	}
	switch decide(gs) {
	case place:
		return c.place(gs)
	case grantReady:
		return c.Report(Report{Namespace: gs.Namespace, Name: gs.Name, Event: Readied})
	case startDelete:
		return c.Delete(gs.Namespace, gs.Name)
	case forget:
		return ignoreNotFound(c.servers.Delete(gs.Namespace, gs.Name))
	}
	return nil
}

func (c *Controller) place(gs *agonesv1.GameServer) error {
	placed, ok := scheduling.Place(gs, c.nodes.List(""), c.servers.List(""))
	if !ok {
		slog.Info("no node has room, waiting", "gameserver", gs.Namespace+"/"+gs.Name)
		return nil
	}
	placed.Status.State, _ = Next(gs.Status.State, Placed)
	_, err := c.servers.Update(placed)
	return err
}

// Delete asks for a GameServer to be removed. The record stays until its
// containers are gone, which the agent reports with ContainersRemoved.
func (c *Controller) Delete(namespace, name string) error {
	return retryOnConflict(func() error {
		gs, ok := c.servers.Get(namespace, name)
		if !ok || gs.DeletionTimestamp != nil {
			return nil
		}
		now := metav1.NewTime(c.now())
		gs.DeletionTimestamp = &now
		_, err := c.servers.Update(gs)
		return err
	})
}

// Report applies an Event to a GameServer. Events that do not apply in the
// GameServer's current state are ignored.
func (c *Controller) Report(r Report) error {
	return retryOnConflict(func() error { return c.report(r) })
}

func (c *Controller) report(r Report) error {
	gs, ok := c.servers.Get(r.Namespace, r.Name)
	if !ok {
		return nil
	}
	if r.Event == ContainersRemoved {
		return c.forgetDeleted(gs)
	}
	next, ok := Next(gs.Status.State, r.Event)
	if !ok || gs.DeletionTimestamp != nil {
		return nil
	}
	node, _ := c.nodes.Get("", gs.Status.NodeName)
	apply(gs, next, r, node, c.now())
	_, err := c.servers.Update(gs)
	return err
}

func (c *Controller) forgetDeleted(gs *agonesv1.GameServer) error {
	if gs.DeletionTimestamp == nil {
		return nil
	}
	return ignoreNotFound(c.servers.Delete(gs.Namespace, gs.Name))
}

// apply moves gs to next and records what comes with the move.
func apply(gs *agonesv1.GameServer, next agonesv1.GameServerState, r Report, node *litev1.Node, now time.Time) {
	gs.Status.State = next
	gs.Status.ReservedUntil = reservedUntil(next, r.Reserve, now)
	if r.Event == SDKAllocate {
		stamp, _ := now.UTC().MarshalText()
		metav1.SetMetaDataAnnotation(&gs.ObjectMeta, LastAllocatedAnnotation, string(stamp))
	}
	if node != nil && reachable(next) {
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
// the address must be published. Ready is included for a game server that
// called Ready before its agent reported it running.
func reachable(s agonesv1.GameServerState) bool {
	return s == agonesv1.GameServerStateScheduled || s == agonesv1.GameServerStateReady
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

// retryOnConflict repeats fn while it loses the race to update a resource.
func retryOnConflict(fn func() error) error {
	var err error
	for range conflictRetries {
		if err = fn(); !errors.Is(err, store.ErrConflict) {
			return err
		}
	}
	return err
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

// SetMetadata adds labels and annotations a game server set through the SDK.
func (c *Controller) SetMetadata(namespace, name string, labels, annotations map[string]string) error {
	return retryOnConflict(func() error {
		gs, ok := c.servers.Get(namespace, name)
		if !ok || gs.DeletionTimestamp != nil {
			return nil
		}
		for key, value := range labels {
			metav1.SetMetaDataLabel(&gs.ObjectMeta, SDKPrefix+key, value)
		}
		for key, value := range annotations {
			metav1.SetMetaDataAnnotation(&gs.ObjectMeta, SDKPrefix+key, value)
		}
		_, err := c.servers.Update(gs)
		return err
	})
}
