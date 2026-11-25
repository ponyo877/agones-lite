package nodes

import (
	"fmt"
	"slices"
	"testing"
	"time"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"pgregory.net/rapid"

	scrimv1 "github.com/ponyo877/scrim/pkg/apis/scrim/v1"
	"github.com/ponyo877/scrim/pkg/gameservers"
)

// These tests state what must hold for any node and any GameServers on it,
// and let rapid look for a case that breaks it.

var everyState = []agonesv1.GameServerState{
	agonesv1.GameServerStateCreating, agonesv1.GameServerStateStarting, agonesv1.GameServerStateScheduled,
	agonesv1.GameServerStateRequestReady, agonesv1.GameServerStateReady, agonesv1.GameServerStateReserved,
	agonesv1.GameServerStateAllocated, agonesv1.GameServerStateShutdown, agonesv1.GameServerStateUnhealthy,
	agonesv1.GameServerStateError,
}

// someGameServers draws up to six GameServers in any state, some of them
// already being deleted.
func someGameServers(t *rapid.T) []*agonesv1.GameServer {
	var servers []*agonesv1.GameServer
	for i := range rapid.IntRange(0, 6).Draw(t, "servers") {
		gs := inState(fmt.Sprint("gs-", i), rapid.SampledFrom(everyState).Draw(t, "state"))
		if rapid.Bool().Draw(t, "being deleted") {
			gs.DeletionTimestamp = &metav1.Time{Time: noon}
		}
		servers = append(servers, gs)
	}
	return servers
}

// aTime draws a moment around the two graces: before, at and after each.
func aTime(t *rapid.T, label string) time.Duration {
	return rapid.SampledFrom([]time.Duration{
		0, time.Second, DefaultGraces.Unallocated - time.Second, DefaultGraces.Unallocated, DefaultGraces.Unallocated + time.Second,
		DefaultGraces.Allocated - time.Second, DefaultGraces.Allocated, DefaultGraces.Allocated + time.Hour,
	}).Draw(t, label)
}

// A lost connection may be a broken network and no more, with players still
// in their sessions. So nothing is given up before its grace is over, a
// session is waited for the longest, and nothing is given up on a node that
// is connected.
func TestGivingUpWaitsForTheGraceAndLongestForSessions(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		servers := someGameServers(t)
		away := aTime(t, "away")
		node := disconnectedFor(away)
		if rapid.Bool().Draw(t, "connected after all") {
			node.Status.Phase = scrimv1.NodeReady
		}

		lost := GivenUp(node, servers, DefaultGraces, noon)

		for _, gs := range lost {
			require.Equal(t, scrimv1.NodeDisconnected, node.Status.Phase, "%s was given up on a connected node", gs.Name)
			require.Contains(t, servers, gs, "%s is not on this node", gs.Name)
			require.False(t, gameservers.Final(gs.Status.State), "%s is %s: there is nothing left to give up", gs.Name, gs.Status.State)
			require.GreaterOrEqual(t, away, graceOf(gs), "%s (%s) was given up after only %s", gs.Name, gs.Status.State, away)
		}
	})
}

// graceOf is how long a GameServer is waited for.
func graceOf(gs *agonesv1.GameServer) time.Duration {
	if gs.Status.State == agonesv1.GameServerStateAllocated {
		return DefaultGraces.Allocated
	}
	return DefaultGraces.Unallocated
}

// What is given up stays given up as time goes on.
func TestWhatIsGivenUpStaysGivenUp(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		servers := someGameServers(t)
		away := aTime(t, "away")

		lost := GivenUp(disconnectedFor(away), servers, DefaultGraces, noon)
		later := GivenUp(disconnectedFor(away+aTime(t, "later")), servers, DefaultGraces, noon)

		for _, gs := range lost {
			require.Contains(t, later, gs, "%s was given up and then was not", gs.Name)
		}
	})
}

// Draining empties a node without ending a session. Only a node that is
// being drained loses anything, and what it loses has nobody playing on it
// and is not on its way out already.
func TestDrainingSparesSessions(t *testing.T) {
	inUse := []agonesv1.GameServerState{agonesv1.GameServerStateAllocated, agonesv1.GameServerStateReserved}
	rapid.Check(t, func(t *rapid.T) {
		servers := someGameServers(t)
		n := node("node-1")
		n.Spec.Unschedulable = rapid.Bool().Draw(t, "cordoned")
		n.Spec.Drain = rapid.Bool().Draw(t, "draining")

		idle := Drained(n, servers)

		for _, gs := range idle {
			require.True(t, n.Spec.Drain, "%s is shut down on a node nobody drains", gs.Name)
			require.NotContains(t, inUse, gs.Status.State, "%s is %s and was told to shut down", gs.Name, gs.Status.State)
			require.Nil(t, gs.DeletionTimestamp, "%s is on its way out already", gs.Name)
			require.False(t, gameservers.Final(gs.Status.State), "%s is %s already", gs.Name, gs.Status.State)
		}
		if !n.Spec.Drain {
			return
		}
		// And nothing idle is left behind: the node does get empty.
		for _, gs := range servers {
			waiting := !slices.Contains(inUse, gs.Status.State) && !gameservers.Final(gs.Status.State) && gs.DeletionTimestamp == nil
			if waiting {
				require.Contains(t, idle, gs, "%s is %s with nobody on it, and stays", gs.Name, gs.Status.State)
			}
		}
	})
}
