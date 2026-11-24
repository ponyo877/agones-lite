// Package nodes is the control plane's side of its conversation with agents.
//
// For now there is one implementation, Local, for an agent that runs in the
// same process as the control plane. It satisfies agent.ControlPlane with
// direct calls. The networked version, for agents on other machines, will
// sit next to it and satisfy the same interface.
//
// Upstream counterpart: none. Kubernetes keeps track of its nodes for Agones.
package nodes

import (
	"errors"
	"sync"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	litev1 "github.com/ponyo877/agones-lite/pkg/apis/lite/v1"
	"github.com/ponyo877/agones-lite/pkg/gameservers"
	"github.com/ponyo877/agones-lite/pkg/store"
	sidecarv1 "github.com/ponyo877/agones-lite/proto/sidecar/v1"
)

// Local connects an agent straight to the store and the GameServer controller.
type Local struct {
	servers    store.Typed[*agonesv1.GameServer]
	nodes      store.Typed[*litev1.Node]
	controller *gameservers.Controller

	mu      sync.Mutex
	readers map[string]LogReader // who reads logs on each connected node
}

// NewLocal returns a Local working on s.
func NewLocal(s *store.Store, controller *gameservers.Controller) *Local {
	return &Local{
		servers:    store.GameServersOf(s),
		nodes:      store.NodesOf(s),
		controller: controller,
		readers:    map[string]LogReader{},
	}
}

// Register records the node, or refreshes it when it is already known. The
// phase is the caller's to decide: an agent in this process is Ready by
// calling, one on another machine is judged by Server.
func (l *Local) Register(node *litev1.Node) error {
	node = node.DeepCopy()
	node.Status.LastHeartbeatTime = metav1.NewTime(l.nodes.Now())
	existing, ok := l.nodes.Get("", node.Name)
	if !ok {
		_, err := l.nodes.Create(node)
		return err
	}
	// Keep what the operator set, such as a cordon; take the rest from the agent.
	lastHeard, wasGone := existing.Status.LastHeartbeatTime, existing.Status.Phase == litev1.NodeDisconnected
	existing.Status = node.Status
	if wasGone && node.Status.Phase == litev1.NodeDisconnected {
		// Gone before and gone still: it was last heard from when its
		// connection ended. An agent that manages to register but not to
		// connect would otherwise put off for ever the day its GameServers
		// are given up.
		existing.Status.LastHeartbeatTime = lastHeard
	}
	_, err := l.nodes.Update(existing)
	if errors.Is(err, store.ErrConflict) {
		return l.Register(node)
	}
	return err
}

// Assigned returns the GameServers placed on the node. Read straight from
// the store, the answer is always current.
func (l *Local) Assigned(node string) ([]*agonesv1.GameServer, bool) {
	var out []*agonesv1.GameServer
	for _, gs := range l.servers.View("") {
		if gs.Status.NodeName == node {
			out = append(out, gs)
		}
	}
	return out, true
}

// Changes signals after any GameServer changed. Signals that pile up while
// the agent is busy collapse into one.
//
// Should the store ever end this subscription for falling behind, the
// signals stop. The agent still looks at regular intervals, and Assigned
// reads the store itself, so it is slower then but misses nothing.
func (l *Local) Changes() (<-chan struct{}, func()) {
	events, cancel := l.servers.Watch()
	signal := make(chan struct{}, 1)
	go func() {
		for range events {
			select {
			case signal <- struct{}{}:
			default:
			}
		}
	}()
	return signal, cancel
}

// Report passes an Event to the GameServer controller.
func (l *Local) Report(r gameservers.Report) error {
	return l.controller.Report(r)
}

// UpdateCounts passes changes to Counters and Lists to the GameServer
// controller. This is where they leave the wire format behind.
func (l *Local) UpdateCounts(namespace, name, change string, counters []*sidecarv1.CounterUpdate, lists []*sidecarv1.ListUpdate) (*agonesv1.GameServer, error) {
	return l.controller.UpdateCounts(namespace, name, change, counterUpdates(counters), listUpdates(lists))
}

func counterUpdates(wire []*sidecarv1.CounterUpdate) []gameservers.CounterUpdate {
	updates := make([]gameservers.CounterUpdate, 0, len(wire))
	for _, u := range wire {
		updates = append(updates, gameservers.CounterUpdate{
			Name: u.GetName(), Capacity: u.Capacity, Count: u.Count, CountDiff: u.GetCountDiff(),
		})
	}
	return updates
}

func listUpdates(wire []*sidecarv1.ListUpdate) []gameservers.ListUpdate {
	updates := make([]gameservers.ListUpdate, 0, len(wire))
	for _, u := range wire {
		updates = append(updates, gameservers.ListUpdate{
			Name: u.GetName(), Capacity: u.Capacity, Add: u.GetAdd(), Remove: u.GetRemove(),
		})
	}
	return updates
}

// SetMetadata passes SDK labels and annotations to the GameServer controller.
func (l *Local) SetMetadata(namespace, name string, labels, annotations map[string]string) error {
	return l.controller.SetMetadata(namespace, name, labels, annotations)
}
