package gameservers

import (
	"slices"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
)

// Event is something that happened to a GameServer.
type Event string

// Events, grouped by who reports them.
const (
	// Reported by the control plane itself.
	Placed  Event = "Placed"  // a node and host ports were chosen
	Readied Event = "Readied" // a Ready request was granted

	// Reported by the node's agent, from what Docker tells it.
	ContainersCreated Event = "ContainersCreated"
	CreateFailed      Event = "CreateFailed"
	ContainersRunning Event = "ContainersRunning"
	ExitedCleanly     Event = "ExitedCleanly"   // exit code 0
	ExitedWithError   Event = "ExitedWithError" // any other exit code
	ContainersMissing Event = "ContainersMissing"
	ContainersRemoved Event = "ContainersRemoved" // cleanup after deletion is done

	// Reported by the sidecar, from what the game server asks through the SDK.
	SDKReady     Event = "SDKReady"
	SDKAllocate  Event = "SDKAllocate"
	SDKReserve   Event = "SDKReserve"
	SDKShutdown  Event = "SDKShutdown"
	HealthFailed Event = "HealthFailed"
)

// rule is one row of the state machine.
type rule struct {
	// from lists the states the event applies in. Empty means every state
	// that is not final: the SDK may ask for anything at any time.
	from []agonesv1.GameServerState
	to   agonesv1.GameServerState
}

var rules = map[Event]rule{
	Placed:            {from: states(agonesv1.GameServerStatePortAllocation, agonesv1.GameServerStateCreating), to: agonesv1.GameServerStateCreating},
	ContainersCreated: {from: states(agonesv1.GameServerStateCreating), to: agonesv1.GameServerStateStarting},
	CreateFailed:      {from: states(agonesv1.GameServerStateCreating, agonesv1.GameServerStateStarting), to: agonesv1.GameServerStateError},
	ContainersRunning: {from: states(agonesv1.GameServerStateStarting), to: agonesv1.GameServerStateScheduled},
	Readied:           {from: states(agonesv1.GameServerStateRequestReady), to: agonesv1.GameServerStateReady},

	SDKReady:    {to: agonesv1.GameServerStateRequestReady},
	SDKAllocate: {to: agonesv1.GameServerStateAllocated},
	SDKReserve:  {to: agonesv1.GameServerStateReserved},
	SDKShutdown: {to: agonesv1.GameServerStateShutdown},

	ExitedCleanly:     {to: agonesv1.GameServerStateShutdown},
	ExitedWithError:   {to: agonesv1.GameServerStateUnhealthy},
	ContainersMissing: {to: agonesv1.GameServerStateUnhealthy},
	HealthFailed:      {to: agonesv1.GameServerStateUnhealthy},
}

// Next returns the state a GameServer moves to when an event arrives.
// ok is false when the event does not apply, so late or repeated events are
// simply ignored.
func Next(current agonesv1.GameServerState, e Event) (next agonesv1.GameServerState, ok bool) {
	r, known := rules[e]
	if !known || Final(current) {
		return current, false
	}
	if len(r.from) > 0 && !slices.Contains(r.from, current) {
		return current, false
	}
	return r.to, true
}

// Final reports whether a state is one a GameServer never leaves.
func Final(s agonesv1.GameServerState) bool {
	return s == agonesv1.GameServerStateShutdown ||
		s == agonesv1.GameServerStateUnhealthy ||
		s == agonesv1.GameServerStateError
}

func states(s ...agonesv1.GameServerState) []agonesv1.GameServerState {
	return s
}
