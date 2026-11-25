package gameserversets

import (
	"cmp"
	"slices"
	"time"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"

	"github.com/ponyo877/scrim/pkg/gameservers"
)

// One pass does a limited amount of work, so that a large change is made in
// steps and each step sees how the last one went. The numbers are upstream's
// defaults for max-game-server-creations-per-batch,
// max-game-server-deletions-per-batch and max-pod-pending-count
// (cmd/controller/main.go).
const (
	maxCreations = 64
	maxDeletions = 64
	// maxPending is how many GameServers of one set may be starting up at
	// the same time.
	maxPending = 5000
)

// errorDelay is how long a GameServer stays in Error before it is replaced.
// Without it, a set whose GameServers fail at once would replace them as
// fast as it can create them.
//
// Upstream: gameServerErrorDeletionDelay.
const errorDelay = 30 * time.Second

// Actions is what Plan wants done to a GameServerSet's GameServers.
type Actions struct {
	// Create is how many GameServers to add.
	Create int
	// Delete lists the GameServers to remove. Upstream removes all of them
	// the same way, whatever their state: it sets the state to Shutdown and
	// leaves the rest to the GameServer controller (deleteGameServers).
	Delete []*agonesv1.GameServer
}

// Plan decides how to move a GameServerSet toward spec.replicas.
// It only looks at its arguments, so tests need no store and no Docker.
//
// counts says how full each node is; see SortForRemoval.
//
// Upstream: computeReconciliationAction in pkg/gameserversets/controller.go.
func Plan(set *agonesv1.GameServerSet, servers []*agonesv1.GameServer, counts map[string]gameservers.NodeCount, now time.Time) Actions {
	g := group(servers, now)
	have := g.protected + len(g.removable)
	want := int(set.Spec.Replicas)
	return Actions{
		Create: creations(set, want-have, g.pending),
		Delete: removals(set, g, have-want, counts),
	}
}

// groups is the GameServers of a set, grouped by what Plan may do with them.
// A GameServer that is already being deleted is in no group.
type groups struct {
	protected int                    // Allocated or Reserved: counted, never removed
	removable []*agonesv1.GameServer // counted, removed when there are too many
	broken    []*agonesv1.GameServer // not counted, always removed
	pending   int                    // how many of removable are still starting up
}

func group(servers []*agonesv1.GameServer, now time.Time) groups {
	var g groups
	for _, gs := range servers {
		switch {
		case !gs.IsDeletable():
			g.protected++
		case gs.IsBeingDeleted():
			// Already on its way out: its replacement is not held back.
		case broken(gs, now):
			g.broken = append(g.broken, gs)
		default:
			g.removable = append(g.removable, gs)
			if startingUp(gs) {
				g.pending++
			}
		}
	}
	return g
}

// broken reports whether gs has failed and is to be replaced now.
func broken(gs *agonesv1.GameServer, now time.Time) bool {
	switch gs.Status.State {
	case agonesv1.GameServerStateUnhealthy:
		return true
	case agonesv1.GameServerStateError:
		return errorDelayOver(gs, now)
	}
	return false
}

// errorDelayOver reports whether gs has been in Error for errorDelay.
// A GameServer that does not say when it failed is not kept waiting.
//
// Upstream: shouldDeleteErroredGameServer.
func errorDelayOver(gs *agonesv1.GameServer, now time.Time) bool {
	erroredAt, err := time.Parse(time.RFC3339, gs.Annotations[agonesv1.GameServerErroredAtAnnotation])
	if err != nil {
		return true
	}
	return now.Sub(erroredAt) >= errorDelay
}

// startingUp reports whether gs counts toward maxPending. A GameServer
// waiting out errorDelay counts too: upstream does not want a set whose
// GameServers keep failing to create more and more of them.
func startingUp(gs *agonesv1.GameServer) bool {
	switch gs.Status.State {
	case agonesv1.GameServerStatePortAllocation,
		agonesv1.GameServerStateCreating,
		agonesv1.GameServerStateStarting,
		agonesv1.GameServerStateScheduled,
		agonesv1.GameServerStateError:
		return true
	}
	return false
}

// creations is how many of the missing GameServers to add in this pass.
func creations(set *agonesv1.GameServerSet, missing, pending int) int {
	if !set.DeletionTimestamp.IsZero() {
		return 0 // upstream's syncGameServerSet adds nothing to a set that is going away
	}
	return max(0, min(missing, maxCreations, maxPending-pending))
}

// removals lists the GameServers to remove in this pass: the broken ones,
// then as many of the removable ones as the set has too many.
func removals(set *agonesv1.GameServerSet, g groups, surplus int, counts map[string]gameservers.NodeCount) []*agonesv1.GameServer {
	// By name, so that a pass with more to remove than it may takes the
	// same ones whatever order the store handed them over in.
	out := g.broken
	slices.SortFunc(out, func(a, b *agonesv1.GameServer) int { return cmp.Compare(a.Name, b.Name) })
	// Upstream takes one off the surplus for every broken GameServer
	// (deleteCount-- in scheduleDeletion), so that much of the surplus
	// stays until a later pass.
	extra := min(surplus, len(g.removable)) - len(g.broken)
	if extra > 0 {
		sorted := SortForRemoval(set.Spec.Scheduling, g.removable, counts, set.Spec.Priorities)
		out = append(out, sorted[:extra]...)
	}
	return out[:min(len(out), maxDeletions)]
}
