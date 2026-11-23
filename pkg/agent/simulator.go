package agent

import (
	"context"
	"time"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"

	litev1 "github.com/ponyo877/agones-lite/pkg/apis/lite/v1"
	"github.com/ponyo877/agones-lite/pkg/gameservers"
)

// Simulator stands in for an agent, Docker and a well-behaved game server
// all at once. Whatever is placed on its node "starts" at once and calls
// Ready; whatever is deleted is "removed" at once. Nothing real runs.
//
// It exists for tests and for load tests, where a thousand game servers are
// needed and their containers are not.
type Simulator struct {
	Node *litev1.Node
	cp   ControlPlane
}

// NewSimulator returns a Simulator that pretends to be node.
func NewSimulator(node *litev1.Node, cp ControlPlane) *Simulator {
	return &Simulator{Node: node, cp: cp}
}

// Run plays the node until ctx is done.
func (s *Simulator) Run(ctx context.Context) error {
	if err := s.cp.Register(s.Node); err != nil {
		return err
	}
	changes, cancel := s.cp.Changes()
	defer cancel()
	tick := time.NewTicker(reconcilePeriod)
	defer tick.Stop()
	for {
		s.step()
		select {
		case <-ctx.Done():
			return nil
		case <-changes:
		case <-tick.C:
		}
	}
}

// step reports, for each GameServer on the node, what a real node would
// report next.
func (s *Simulator) step() {
	placed, current := s.cp.Assigned(s.Node.Name)
	if !current {
		return
	}
	for _, gs := range placed {
		for _, event := range next(gs) {
			_ = s.cp.Report(gameservers.Report{Namespace: gs.Namespace, Name: gs.Name, Event: event})
		}
	}
}

// next lists what happens to gs on a node where everything goes well.
func next(gs *agonesv1.GameServer) []gameservers.Event {
	switch {
	case gs.DeletionTimestamp != nil:
		return []gameservers.Event{gameservers.ContainersRemoved}
	case gs.Status.State == agonesv1.GameServerStateCreating:
		return []gameservers.Event{gameservers.ContainersCreated, gameservers.ContainersRunning, gameservers.SDKReady}
	}
	return nil
}
