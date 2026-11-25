// Package controlplane puts the control plane together: one store, the
// controllers that work on it, and the APIs in front of it.
//
// Nothing here decides anything. Read it to see which parts exist and how
// they are connected; then read the parts.
package controlplane

import (
	"context"
	"sync"

	"github.com/ponyo877/scrim/pkg/apiserver"
	"github.com/ponyo877/scrim/pkg/control"
	"github.com/ponyo877/scrim/pkg/events"
	"github.com/ponyo877/scrim/pkg/fleetautoscalers"
	"github.com/ponyo877/scrim/pkg/fleets"
	"github.com/ponyo877/scrim/pkg/gameserverallocations"
	"github.com/ponyo877/scrim/pkg/gameservers"
	"github.com/ponyo877/scrim/pkg/gameserversets"
	"github.com/ponyo877/scrim/pkg/metrics"
	"github.com/ponyo877/scrim/pkg/nodes"
	"github.com/ponyo877/scrim/pkg/store"
)

// ControlPlane is every part of the control plane, wired to one store.
type ControlPlane struct {
	// The controllers, from the innermost resource outwards.
	GameServers    *gameservers.Controller
	GameServerSets *gameserversets.Controller
	Fleets         *fleets.Controller
	Autoscalers    *fleetautoscalers.Controller
	Nodes          *nodes.Controller

	// The three ways in.
	API        *apiserver.Server              // for operators
	Allocation *gameserverallocations.Service // for matchmakers
	Agents     *nodes.Local                   // for agents, in or out of this process
}

// New wires a control plane to s. History goes to recorder and measurements
// to measure; either may be nil.
//
// The nodes s remembers all start out Disconnected: see StartOver.
func New(s *store.Store, recorder *events.Recorder, measure *metrics.Metrics, graces nodes.Graces) (*ControlPlane, error) {
	servers := gameservers.NewController(s, recorder)
	servers.Entered = measure.GameServerEntered
	allocation := gameserverallocations.NewService(s, recorder)
	allocation.Observe = measure.Allocated
	s.Saved = measure.Saved
	nodeController := nodes.NewController(s, servers, recorder, graces)
	if err := nodeController.StartOver(); err != nil {
		return nil, err
	}
	agents := nodes.NewLocal(s, servers)
	return &ControlPlane{
		GameServers:    servers,
		GameServerSets: gameserversets.NewController(s, servers, recorder),
		Fleets:         fleets.NewController(s, recorder),
		Autoscalers:    fleetautoscalers.NewController(s, recorder, fleetautoscalers.Post),
		Nodes:          nodeController,
		API:            apiserver.New(s, recorder, nodeController, agents),
		Allocation:     allocation,
		Agents:         agents,
	}, nil
}

// controllers names the controllers, from the innermost resource outwards.
func (cp *ControlPlane) controllers() map[string]control.Controller {
	return map[string]control.Controller{
		"gameservers":      cp.GameServers,
		"gameserversets":   cp.GameServerSets,
		"fleets":           cp.Fleets,
		"fleetautoscalers": cp.Autoscalers,
		"nodes":            cp.Nodes,
	}
}

// Run runs every controller until ctx is done.
func (cp *ControlPlane) Run(ctx context.Context) {
	var running sync.WaitGroup
	for _, c := range cp.controllers() {
		running.Go(func() { control.Run(ctx, c) })
	}
	running.Wait()
}
