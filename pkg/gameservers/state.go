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
	Placed     Event = "Placed"     // a node and host ports were chosen
	Readied    Event = "Readied"    // a Ready request was granted
	Allocated  Event = "Allocated"  // an allocation request was given it
	ScaledDown Event = "ScaledDown" // its GameServerSet has one too many
	NodeLost   Event = "NodeLost"   // its node has been out of reach for too long

	// Reported by the node's agent, from what Docker tells it.
	ContainersCreated Event = "ContainersCreated"
	CreateFailed      Event = "CreateFailed"
	ContainersRunning Event = "ContainersRunning"
	ExitedCleanly     Event = "ExitedCleanly"   // exit code 0
	ExitedWithError   Event = "ExitedWithError" // any other exit code
	ContainersMissing Event = "ContainersMissing"
	ContainersRemoved Event = "ContainersRemoved" // cleanup after deletion is done

	// Reported by the sidecar: what the game server asks through the SDK,
	// and what the sidecar notices by itself.
	SDKReady       Event = "SDKReady"
	SDKAllocate    Event = "SDKAllocate"
	SDKReserve     Event = "SDKReserve"
	SDKShutdown    Event = "SDKShutdown"
	HealthFailed   Event = "HealthFailed"
	ReserveExpired Event = "ReserveExpired" // the time of an SDKReserve is up
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
	// A matchmaker gets a GameServer that waits for a session, or one that
	// has a session already when its request asks for that: never one that
	// is still coming up, and never one a game server holds back for itself.
	Allocated: {from: states(agonesv1.GameServerStateReady, agonesv1.GameServerStateAllocated), to: agonesv1.GameServerStateAllocated},
	// Never from Allocated or Reserved: a session may have started since the
	// decision to scale down was taken.
	ScaledDown: {from: beforeSession, to: agonesv1.GameServerStateShutdown},

	SDKReady:    {to: agonesv1.GameServerStateRequestReady},
	SDKAllocate: {to: agonesv1.GameServerStateAllocated},
	SDKReserve:  {to: agonesv1.GameServerStateReserved},
	SDKShutdown: {to: agonesv1.GameServerStateShutdown},
	// Only from Reserved, unlike SDKReady: the sidecar's timer may fire just
	// as the game server allocates itself, and must not end that session.
	ReserveExpired: {from: states(agonesv1.GameServerStateReserved), to: agonesv1.GameServerStateRequestReady},

	ExitedCleanly:     {to: agonesv1.GameServerStateShutdown},
	ExitedWithError:   {to: agonesv1.GameServerStateUnhealthy},
	ContainersMissing: {to: agonesv1.GameServerStateUnhealthy},
	HealthFailed:      {to: agonesv1.GameServerStateUnhealthy},
	NodeLost:          {to: agonesv1.GameServerStateUnhealthy},
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

// beforeSession lists the states in which no players are, or are about to be, on the server.
var beforeSession = states(
	agonesv1.GameServerStatePortAllocation, agonesv1.GameServerStateCreating,
	agonesv1.GameServerStateStarting, agonesv1.GameServerStateScheduled,
	agonesv1.GameServerStateRequestReady, agonesv1.GameServerStateReady,
)

func states(s ...agonesv1.GameServerState) []agonesv1.GameServerState {
	return s
}
