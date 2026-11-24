package fleetautoscalers

import (
	"context"
	"errors"
	"fmt"
	"time"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	autoscalingv1 "agones.dev/agones/pkg/apis/autoscaling/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"

	"github.com/ponyo877/agones-lite/pkg/control"
	"github.com/ponyo877/agones-lite/pkg/events"
	"github.com/ponyo877/agones-lite/pkg/gameservers"
	"github.com/ponyo877/agones-lite/pkg/store"
)

const (
	// Kind is the name FleetAutoscalers go by in manifests and in the event history.
	Kind = "FleetAutoscaler"
	// tick is how often the controller checks whose turn it is. Each
	// FleetAutoscaler has its own interval, a multiple of a second.
	tick = time.Second
)

// Controller applies every FleetAutoscaler to its Fleet, each at its own
// interval. It is the thin half of this package: what a policy wants is
// worked out in desired.go.
type Controller struct {
	scalers store.Typed[*autoscalingv1.FleetAutoscaler]
	fleets  store.Typed[*agonesv1.Fleet]
	servers store.Typed[*agonesv1.GameServer]
	events  *events.Recorder
	call    Webhook
	now     func() time.Time
	// passed remembers each FleetAutoscaler's last pass: when it was, and
	// which generation of the spec it saw. Only the sync loop touches it.
	passed map[store.Key]visit
}

// visit is one pass over a FleetAutoscaler.
type visit struct {
	at         time.Time
	generation int64
}

// NewController returns a Controller working on s. Webhook policies are
// sent with call; Post is the one that really talks HTTP.
func NewController(s *store.Store, recorder *events.Recorder, call Webhook) *Controller {
	return &Controller{
		scalers: store.FleetAutoscalersOf(s),
		fleets:  store.FleetsOf(s),
		servers: store.GameServersOf(s),
		events:  recorder,
		call:    call,
		now:     s.Now,
		passed:  map[store.Key]visit{},
	}
}

// Loop says what the controller looks at, and when. Run runs it; a test may
// step through it with a control.Stepper instead.
func (c *Controller) Loop() (control.Loop, []control.Watch) {
	return control.Loop{Sync: c.sync, All: c.all, Resync: tick}, []control.Watch{
		{Subscribe: c.scalers.Watch, Keys: control.Self},
	}
}

// Run applies the FleetAutoscalers until ctx is done.
func (c *Controller) Run(ctx context.Context) {
	control.Run(ctx, c)
}

func (c *Controller) all() []store.Key {
	var keys []store.Key
	for _, fas := range c.scalers.View("") {
		keys = append(keys, store.KeyOf(store.FleetAutoscalers, fas))
	}
	return keys
}

func (c *Controller) sync(_ context.Context, key store.Key) error {
	fas, ok := c.scalers.Get(key.Namespace, key.Name)
	if !ok {
		delete(c.passed, key)
		return nil
	}
	if fas.DeletionTimestamp != nil {
		return c.remove(key)
	}
	if !due(fas, c.passed[key], c.now()) {
		return nil
	}
	c.passed[key] = visit{at: c.now(), generation: fas.Generation}
	return c.publish(fas, c.apply(fas))
}

// remove finishes the deletion of a FleetAutoscaler. It owns nothing that
// would have to go first, and its Fleet stays at the size it has.
func (c *Controller) remove(key store.Key) error {
	delete(c.passed, key)
	err := c.scalers.Delete(key.Namespace, key.Name)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	return err
}

// due reports whether it is time for another pass: the interval is over, or
// the FleetAutoscaler was edited since the last one.
func due(fas *autoscalingv1.FleetAutoscaler, last visit, now time.Time) bool {
	interval := time.Duration(fas.Spec.Sync.FixedInterval.Seconds) * time.Second
	return last.generation != fas.Generation || now.Sub(last.at) >= interval
}

// apply runs one pass for fas and returns the status to record.
func (c *Controller) apply(fas *autoscalingv1.FleetAutoscaler) autoscalingv1.FleetAutoscalerStatus {
	fleet, ok := c.fleets.Get(fas.Namespace, fas.Spec.FleetName)
	if !ok || fleet.DeletionTimestamp != nil {
		c.events.Record(Kind, fas.Namespace, fas.Name, events.Warning, "FailedGetFleet", "fleet %q not found", fas.Spec.FleetName)
		return StatusUnableToScale(fas)
	}
	all := c.servers.View("")
	desired, limited, applied, err := Desired(fas.Spec.Policy, fleet, ofFleet(all, fleet), gameservers.CountByNode(all), c.now(), c.call)
	if err != nil {
		c.events.Record(Kind, fas.Namespace, fas.Name, events.Warning, "FleetAutoscaler", "%v", err)
		return StatusUnableToScale(fas)
	}
	scaled := desired != fleet.Spec.Replicas
	if scaled {
		if err := c.scale(fleet, desired); err != nil {
			c.events.Record(Kind, fas.Namespace, fas.Name, events.Warning, "AutoScalingFleetError",
				"Error on scaling fleet %s from %d to %d. Error: %v", fleet.Name, fleet.Spec.Replicas, desired, err)
			return StatusUnableToScale(fas)
		}
		c.events.Record(Kind, fas.Namespace, fas.Name, events.Normal, "AutoScalingFleet",
			"Scaling fleet %s from %d to %d", fleet.Name, fleet.Spec.Replicas, desired)
	}
	return Status(fas, fleet.Status.Replicas, desired, scaled, limited, applied, c.now())
}

// ofFleet picks the GameServers that belong to fleet.
func ofFleet(all []*agonesv1.GameServer, fleet *agonesv1.Fleet) []*agonesv1.GameServer {
	var out []*agonesv1.GameServer
	for _, gs := range all {
		if gs.Namespace == fleet.Namespace && gs.Labels[agonesv1.FleetNameLabel] == fleet.Name {
			out = append(out, gs)
		}
	}
	return out
}

// scale writes the Fleet's new size. It goes past admission, so it refuses
// for itself what admission would: the size no policy may ask for.
func (c *Controller) scale(fleet *agonesv1.Fleet, replicas int32) error {
	if replicas < 0 {
		return fmt.Errorf("a Fleet cannot have %d replicas", replicas)
	}
	return c.fleets.Change(fleet.Namespace, fleet.Name, func(current *agonesv1.Fleet) bool {
		current.Spec.Replicas = replicas
		current.Generation++
		return true
	})
}

// publish writes the FleetAutoscaler's status when it changed.
func (c *Controller) publish(fas *autoscalingv1.FleetAutoscaler, status autoscalingv1.FleetAutoscalerStatus) error {
	if apiequality.Semantic.DeepEqual(fas.Status, status) {
		return nil
	}
	return c.scalers.Change(fas.Namespace, fas.Name, func(current *autoscalingv1.FleetAutoscaler) bool {
		current.Status = status
		return true
	})
}
