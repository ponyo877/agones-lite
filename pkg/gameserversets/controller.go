package gameserversets

import (
	"context"
	"errors"
	"time"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ponyo877/agones-lite/pkg/control"
	"github.com/ponyo877/agones-lite/pkg/events"
	"github.com/ponyo877/agones-lite/pkg/gameservers"
	"github.com/ponyo877/agones-lite/pkg/store"
)

const (
	// Kind is the name GameServerSets go by in manifests and in the event history.
	Kind = "GameServerSet"
	// resyncPeriod is how often every set is looked at again.
	resyncPeriod = 30 * time.Second
)

// Controller carries out what Plan decides. It is the thin half of this
// package: everything that needs thought is in plan.go.
type Controller struct {
	sets        store.Typed[*agonesv1.GameServerSet]
	servers     store.Typed[*agonesv1.GameServer]
	gameServers *gameservers.Controller
	events      *events.Recorder
	now         func() time.Time
}

// NewController returns a Controller working on s. GameServers are removed
// through their own controller, which knows how.
func NewController(s *store.Store, gameServers *gameservers.Controller, recorder *events.Recorder) *Controller {
	return &Controller{
		sets:        store.GameServerSetsOf(s),
		servers:     store.GameServersOf(s),
		gameServers: gameServers,
		events:      recorder,
		now:         s.Now,
	}
}

// Loop says what the controller looks at, and when. Run runs it; a test may
// step through it with a control.Stepper instead.
func (c *Controller) Loop() (control.Loop, []control.Watch) {
	return control.Loop{Sync: c.sync, All: c.all, Resync: resyncPeriod}, []control.Watch{
		{Subscribe: c.sets.Watch, Keys: control.Self},
		{Subscribe: c.servers.Watch, Keys: ownerOf},
	}
}

// Run keeps every GameServerSet at its size until ctx is done.
func (c *Controller) Run(ctx context.Context) {
	control.Run(ctx, c)
}

func (c *Controller) all() []store.Key {
	var keys []store.Key
	for _, set := range c.sets.View("") {
		keys = append(keys, store.KeyOf(store.GameServerSets, set))
	}
	return keys
}

// ownerOf names the set a changed GameServer belongs to.
func ownerOf(e store.Event) []store.Key {
	name := e.Object.GetLabels()[agonesv1.GameServerSetGameServerLabel]
	if name == "" {
		return nil
	}
	return []store.Key{{Kind: store.GameServerSets, Namespace: e.Key.Namespace, Name: name}}
}

func (c *Controller) sync(_ context.Context, key store.Key) error {
	set, ok := c.sets.Get(key.Namespace, key.Name)
	if !ok {
		return nil
	}
	owned := c.owned(set)
	if set.DeletionTimestamp != nil {
		return c.dissolve(set, owned)
	}
	actions := Plan(set, owned, gameservers.CountByNode(c.servers.View("")), c.now())
	// The Fleet and the autoscaler steer by the status, so it is written
	// from a fresh look, and also when the plan could only be carried out
	// in part: sessions begin and end whether or not this pass went well.
	//
	// Upstream writes it from the list it planned with, and not at all
	// after a failure (syncGameServerSet).
	return errors.Join(c.carryOut(set, actions), c.publish(set, c.owned(set)))
}

// owned returns the GameServers that set controls.
func (c *Controller) owned(set *agonesv1.GameServerSet) []*agonesv1.GameServer {
	var out []*agonesv1.GameServer
	for _, gs := range c.servers.View(set.Namespace) {
		if metav1.IsControlledBy(gs, set) {
			out = append(out, gs)
		}
	}
	return out
}

// dissolve removes a set that is being deleted: first all its GameServers,
// Allocated ones included, then the set itself. Kubernetes' garbage
// collector does the same for upstream.
func (c *Controller) dissolve(set *agonesv1.GameServerSet, owned []*agonesv1.GameServer) error {
	for _, gs := range owned {
		if err := c.gameServers.Delete(gs.Namespace, gs.Name); err != nil {
			return err
		}
	}
	if len(owned) > 0 {
		return nil // looked at again as each GameServer goes
	}
	err := c.sets.Delete(set.Namespace, set.Name)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	return err
}

func (c *Controller) carryOut(set *agonesv1.GameServerSet, actions Actions) error {
	for range actions.Create {
		gs := set.GameServer()
		gs.APIVersion, gs.Kind = "agones.dev/v1", gameservers.Kind
		gs.ApplyDefaults()
		if _, err := c.servers.Create(gs); err != nil {
			return err
		}
	}
	for _, gs := range actions.Delete {
		if err := c.remove(gs); err != nil {
			return err
		}
	}
	if actions.Create > 0 || len(actions.Delete) > 0 {
		c.events.Record(Kind, set.Namespace, set.Name, events.Normal, "Scaled",
			"created %d and removed %d GameServers", actions.Create, len(actions.Delete))
	}
	return nil
}

// remove takes one GameServer out of the set. Upstream writes Shutdown over
// whatever state it has. Here Unhealthy and Error are final states, so those
// are deleted outright, and the others are told to shut down.
func (c *Controller) remove(gs *agonesv1.GameServer) error {
	if gameservers.Final(gs.Status.State) {
		return c.gameServers.Delete(gs.Namespace, gs.Name)
	}
	return c.gameServers.Report(gameservers.Report{Namespace: gs.Namespace, Name: gs.Name, Event: gameservers.ScaledDown})
}

// publish writes the set's status when it changed.
func (c *Controller) publish(set *agonesv1.GameServerSet, owned []*agonesv1.GameServer) error {
	status := Status(set, owned)
	if apiequality.Semantic.DeepEqual(set.Status, status) {
		return nil
	}
	return c.sets.Change(set.Namespace, set.Name, func(current *agonesv1.GameServerSet) bool {
		current.Status = status
		return true
	})
}
