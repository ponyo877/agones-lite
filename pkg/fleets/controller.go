package fleets

import (
	"context"
	"errors"
	"time"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ponyo877/agones-lite/pkg/control"
	"github.com/ponyo877/agones-lite/pkg/events"
	"github.com/ponyo877/agones-lite/pkg/store"
)

const (
	// Kind is the name Fleets go by in manifests and in the event history.
	Kind = "Fleet"
	// resyncPeriod is how often every Fleet is looked at again.
	resyncPeriod = 30 * time.Second
)

// Controller carries out what Plan decides. It is the thin half of this
// package: everything that needs thought is in plan.go.
type Controller struct {
	fleets store.Typed[*agonesv1.Fleet]
	sets   store.Typed[*agonesv1.GameServerSet]
	events *events.Recorder
	now    func() time.Time
}

// NewController returns a Controller working on s.
func NewController(s *store.Store, recorder *events.Recorder) *Controller {
	return &Controller{
		fleets: store.FleetsOf(s),
		sets:   store.GameServerSetsOf(s),
		events: recorder,
		now:    time.Now,
	}
}

// Run keeps every Fleet's GameServerSets in shape until ctx is done.
func (c *Controller) Run(ctx context.Context) {
	loop := control.Loop{Sync: c.sync, All: c.all, Resync: resyncPeriod}
	loop.Run(ctx,
		control.Watch{Subscribe: c.fleets.Watch, Keys: control.Self},
		control.Watch{Subscribe: c.sets.Watch, Keys: ownerOf},
	)
}

func (c *Controller) all() []store.Key {
	var keys []store.Key
	for _, fleet := range c.fleets.List("") {
		keys = append(keys, store.KeyOf(store.Fleets, fleet))
	}
	return keys
}

// ownerOf names the Fleet a changed GameServerSet belongs to.
func ownerOf(e store.Event) []store.Key {
	name := e.Object.GetLabels()[agonesv1.FleetNameLabel]
	if name == "" {
		return nil
	}
	return []store.Key{{Kind: store.Fleets, Namespace: e.Key.Namespace, Name: name}}
}

func (c *Controller) sync(_ context.Context, key store.Key) error {
	fleet, ok := c.fleets.Get(key.Namespace, key.Name)
	if !ok {
		return nil
	}
	owned := c.owned(fleet)
	if fleet.DeletionTimestamp != nil {
		return c.dissolve(fleet, owned)
	}
	// A set that is on its way out no longer counts. Upstream never sees
	// one: Kubernetes removes the object at once and cleans up behind it.
	owned = remaining(owned)
	changes, err := Plan(fleet, owned)
	if err != nil {
		c.events.Record(Kind, fleet.Namespace, fleet.Name, events.Warning, "Invalid", "%v", err)
		return nil // trying again would fail the same way
	}
	if err := c.carryOut(fleet, changes); err != nil {
		return err
	}
	return c.publish(fleet, owned)
}

// owned returns the GameServerSets that fleet controls.
func (c *Controller) owned(fleet *agonesv1.Fleet) []*agonesv1.GameServerSet {
	var out []*agonesv1.GameServerSet
	for _, set := range c.sets.List(fleet.Namespace) {
		if metav1.IsControlledBy(set, fleet) {
			out = append(out, set)
		}
	}
	return out
}

// remaining leaves out the sets that are being deleted.
func remaining(sets []*agonesv1.GameServerSet) []*agonesv1.GameServerSet {
	var out []*agonesv1.GameServerSet
	for _, set := range sets {
		if set.DeletionTimestamp == nil {
			out = append(out, set)
		}
	}
	return out
}

// dissolve removes a Fleet that is being deleted: first its sets, which
// take their GameServers with them, then the Fleet itself.
func (c *Controller) dissolve(fleet *agonesv1.Fleet, owned []*agonesv1.GameServerSet) error {
	for _, set := range owned {
		if err := c.sets.MarkDeleted(set.Namespace, set.Name, c.now()); err != nil {
			return err
		}
	}
	if len(owned) > 0 {
		return nil // looked at again as each set goes
	}
	err := c.fleets.Delete(fleet.Namespace, fleet.Name)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	return err
}

func (c *Controller) carryOut(fleet *agonesv1.Fleet, changes Changes) error {
	if err := c.replaceActive(fleet, changes); err != nil {
		return err
	}
	for name, replicas := range changes.Scale {
		if err := c.scale(fleet, name, replicas); err != nil {
			return err
		}
	}
	for _, name := range changes.Delete {
		if err := c.sets.MarkDeleted(fleet.Namespace, name, c.now()); err != nil {
			return err
		}
		c.events.Record(Kind, fleet.Namespace, fleet.Name, events.Normal, "DeletingGameServerSet", "%s is empty", name)
	}
	return nil
}

// replaceActive creates the set for a new template, or brings the current
// one in line with the Fleet.
func (c *Controller) replaceActive(fleet *agonesv1.Fleet, changes Changes) error {
	if set := changes.Create; set != nil {
		set.APIVersion, set.Kind = "agones.dev/v1", "GameServerSet"
		if _, err := c.sets.Create(set); err != nil {
			return err
		}
		c.events.Record(Kind, fleet.Namespace, fleet.Name, events.Normal, "CreatingGameServerSet", "a new GameServerSet for the current template")
	}
	if changes.Update == nil {
		return nil
	}
	_, err := c.sets.Update(changes.Update)
	return err
}

func (c *Controller) scale(fleet *agonesv1.Fleet, set string, replicas int32) error {
	var from int32
	err := c.sets.Change(fleet.Namespace, set, func(current *agonesv1.GameServerSet) bool {
		from = current.Spec.Replicas
		current.Spec.Replicas = replicas
		return from != replicas
	})
	if err == nil && from != replicas {
		c.events.Record(Kind, fleet.Namespace, fleet.Name, events.Normal, "ScalingGameServerSet", "%s from %d to %d", set, from, replicas)
	}
	return err
}

// publish writes the Fleet's status when it changed.
func (c *Controller) publish(fleet *agonesv1.Fleet, owned []*agonesv1.GameServerSet) error {
	status := Status(fleet, owned)
	if apiequality.Semantic.DeepEqual(fleet.Status, status) {
		return nil
	}
	return c.fleets.Change(fleet.Namespace, fleet.Name, func(current *agonesv1.Fleet) bool {
		current.Status = status
		return true
	})
}
