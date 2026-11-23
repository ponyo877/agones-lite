package gameservers

import (
	"testing"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	"github.com/stretchr/testify/assert"
)

const (
	portAllocation = agonesv1.GameServerStatePortAllocation
	creating       = agonesv1.GameServerStateCreating
	starting       = agonesv1.GameServerStateStarting
	scheduled      = agonesv1.GameServerStateScheduled
	requestReady   = agonesv1.GameServerStateRequestReady
	ready          = agonesv1.GameServerStateReady
	reserved       = agonesv1.GameServerStateReserved
	allocated      = agonesv1.GameServerStateAllocated
	shutdown       = agonesv1.GameServerStateShutdown
	unhealthy      = agonesv1.GameServerStateUnhealthy
	failed         = agonesv1.GameServerStateError
)

func TestNext(t *testing.T) {
	tests := []struct {
		from  agonesv1.GameServerState
		event Event
		to    agonesv1.GameServerState
		ok    bool
	}{
		// The ordinary path from creation to Ready.
		{portAllocation, Placed, creating, true},
		{creating, Placed, creating, true},
		{creating, ContainersCreated, starting, true},
		{starting, ContainersRunning, scheduled, true},
		{scheduled, SDKReady, requestReady, true},
		{requestReady, Readied, ready, true},

		// A session, and going back to the pool afterwards.
		{ready, SDKAllocate, allocated, true},
		{ready, SDKReserve, reserved, true},
		{reserved, SDKReady, requestReady, true},
		{allocated, SDKReady, requestReady, true},
		{allocated, SDKShutdown, shutdown, true},

		// A game server that calls Ready before its agent reports it running.
		{starting, SDKReady, requestReady, true},
		{ready, ContainersRunning, ready, false},

		// Failures.
		{creating, CreateFailed, failed, true},
		{ready, CreateFailed, ready, false},
		{ready, HealthFailed, unhealthy, true},
		{allocated, ExitedWithError, unhealthy, true},
		{ready, ContainersMissing, unhealthy, true},
		{allocated, ExitedCleanly, shutdown, true},

		// Events in the wrong state are ignored.
		{ready, Placed, ready, false},
		{ready, Readied, ready, false},
		{ready, Event("NoSuchEvent"), ready, false},

		// Final states stay final.
		{shutdown, SDKReady, shutdown, false},
		{unhealthy, SDKReady, unhealthy, false},
		{unhealthy, ExitedCleanly, unhealthy, false},
		{failed, SDKShutdown, failed, false},
	}
	for _, tc := range tests {
		t.Run(string(tc.from)+"+"+string(tc.event), func(t *testing.T) {
			to, ok := Next(tc.from, tc.event)
			assert.Equal(t, tc.to, to)
			assert.Equal(t, tc.ok, ok)
		})
	}
}
