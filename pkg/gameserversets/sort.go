package gameserversets

import (
	"slices"
	"sort"

	"agones.dev/agones/pkg/apis"
	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"

	"github.com/ponyo877/agones-lite/pkg/gameservers"
)

// SortForRemoval orders servers so that the ones to remove first come first.
// It returns a new slice and leaves servers as it was.
//
// Packed empties the least full nodes first. counts says how full each node
// is, counting the GameServers of every set, as upstream's PerNodeCounter
// does. Every other strategy is treated as Distributed, which removes the
// oldest GameServers first.
//
// Upstream: SortGameServersByStrategy in pkg/gameserversets/gameserversets.go.
func SortForRemoval(strategy apis.SchedulingStrategy, servers []*agonesv1.GameServer, counts map[string]gameservers.NodeCount, priorities []agonesv1.Priority) []*agonesv1.GameServer {
	before := oldestFirst(priorities)
	if strategy == apis.Packed {
		before = emptiestNodeFirst(counts, priorities)
	}
	sorted := slices.Clone(servers)
	// sort.Slice, as upstream, so that ties fall the same way.
	sort.Slice(sorted, func(i, j int) bool { return before(sorted[i], sorted[j]) })
	return sorted
}

// order reports whether a is removed before b.
type order func(a, b *agonesv1.GameServer) bool

// emptiestNodeFirst is the order for Packed: scaling down should free whole
// nodes, so GameServers on the least full nodes go first.
//
// Upstream: sortGameServersByPackedStrategy.
func emptiestNodeFirst(counts map[string]gameservers.NodeCount, priorities []agonesv1.Priority) order {
	return func(a, b *agonesv1.GameServer) bool {
		// A GameServer that is on no node yet, or whose node is gone, goes
		// first. Upstream says so even when b is such a GameServer too, so
		// their order among themselves is left to the sort.
		countA, known := counts[a.Status.NodeName]
		if !known {
			return true
		}
		countB, known := counts[b.Status.NodeName]
		if !known {
			return false
		}
		if a.Status.NodeName == b.Status.NodeName {
			return beforeOnSameNode(a, b, priorities)
		}
		if fullA, fullB := countA.Ready+countA.Allocated, countB.Ready+countB.Allocated; fullA != fullB {
			return fullA < fullB
		}
		// Equally full nodes go by name, so that one is emptied before the
		// other is touched.
		return a.Status.NodeName < b.Status.NodeName
	}
}

// beforeOnSameNode orders two GameServers that share a node. One that is
// not Ready yet goes before one that is; then the priorities decide, then
// the name.
func beforeOnSameNode(a, b *agonesv1.GameServer, priorities []agonesv1.Priority) bool {
	if a.IsBeforeReady() && b.Status.State == agonesv1.GameServerStateReady {
		return true
	}
	if b.IsBeforeReady() && a.Status.State == agonesv1.GameServerStateReady {
		return false
	}
	if first := a.CompareCountAndListPriorities(priorities, b); first != nil {
		return *first
	}
	return a.Name < b.Name
}

// oldestFirst is the order for Distributed: the priorities decide, then age.
//
// Upstream: sortGameServersByDistributedStrategy. Its comment says newest
// first, but its code puts the oldest first, and this follows the code.
func oldestFirst(priorities []agonesv1.Priority) order {
	return func(a, b *agonesv1.GameServer) bool {
		if first := a.CompareCountAndListPriorities(priorities, b); first != nil {
			return *first
		}
		return a.CreationTimestamp.Before(&b.CreationTimestamp)
	}
}
