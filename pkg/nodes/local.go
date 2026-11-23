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
	"time"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	litev1 "github.com/ponyo877/agones-lite/pkg/apis/lite/v1"
	"github.com/ponyo877/agones-lite/pkg/gameservers"
	"github.com/ponyo877/agones-lite/pkg/store"
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

// Register records the node, or refreshes it when it is already known.
func (l *Local) Register(node *litev1.Node) error {
	node = node.DeepCopy()
	node.Status.LastHeartbeatTime = metav1.NewTime(time.Now())
	existing, ok := l.nodes.Get("", node.Name)
	if !ok {
		_, err := l.nodes.Create(node)
		return err
	}
	// Keep what the operator set, such as a cordon; take the rest from the agent.
	existing.Status = node.Status
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
	for _, gs := range l.servers.List("") {
		if gs.Status.NodeName == node {
			out = append(out, gs)
		}
	}
	return out, true
}

// Changes signals after any GameServer changed. Signals that pile up while
// the agent is busy collapse into one.
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

// SetMetadata passes SDK labels and annotations to the GameServer controller.
func (l *Local) SetMetadata(namespace, name string, labels, annotations map[string]string) error {
	return l.controller.SetMetadata(namespace, name, labels, annotations)
}
