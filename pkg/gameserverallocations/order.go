package gameserverallocations

import (
	"cmp"

	"agones.dev/agones/pkg/apis"
	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	allocationv1 "agones.dev/agones/pkg/apis/allocation/v1"

	"github.com/ponyo877/scrim/pkg/gameservers"
)

// order compares two candidates. It is negative when a should be offered to
// the request before b, the way slices.SortFunc expects.
type order func(a, b *agonesv1.GameServer) int

// orderOf returns the order gsa's scheduling strategy asks for. Like
// upstream, every strategy other than Packed is sorted as Distributed.
func orderOf(gsa *allocationv1.GameServerAllocation, counts map[string]gameservers.NodeCount) order {
	if gsa.Spec.Scheduling == apis.Packed {
		return packed(gsa.Spec.Priorities, counts)
	}
	return distributed(gsa.Spec.Priorities)
}

// packed is the order for Packed: sessions go to the nodes that are already
// the busiest, so that the quiet ones empty out and can be removed. A node
// missing from counts would rank as an empty one, and so last; Find leaves
// the GameServers on such a node out before it sorts.
//
// Upstream: ListSortedGameServers in
// pkg/gameserverallocations/allocation_cache.go.
func packed(priorities []agonesv1.Priority, counts map[string]gameservers.NodeCount) order {
	return func(a, b *agonesv1.GameServer) int {
		nodeA, nodeB := counts[a.Status.NodeName], counts[b.Status.NodeName]
		return cmp.Or(
			allocatedFirst(a, b),
			cmp.Compare(nodeB.Allocated, nodeA.Allocated),
			cmp.Compare(nodeB.Ready, nodeA.Ready),
			byPriorities(priorities, a, b),
			cmp.Compare(a.Name, b.Name),
		)
	}
}

// distributed is the order for Distributed when the request has priorities.
// Without them it only makes pick's choice repeatable.
//
// Upstream: ListSortedGameServersPriorities in the same file.
func distributed(priorities []agonesv1.Priority) order {
	return func(a, b *agonesv1.GameServer) int {
		return cmp.Or(
			byPriorities(priorities, a, b),
			cmp.Compare(a.Name, b.Name),
		)
	}
}

// allocatedFirst is upstream's "search Allocated GameServers first". A
// selector that names a state never sees the difference, and defaulting gives
// every selector a state, so this only keeps the order the same as upstream's.
func allocatedFirst(a, b *agonesv1.GameServer) int {
	switch a.Status.State {
	case b.Status.State:
		return 0
	case agonesv1.GameServerStateAllocated:
		return -1
	}
	return 1
}

// byPriorities compares by the available capacity of the counters and lists
// the request names. It is 0 when they do not tell a and b apart.
func byPriorities(priorities []agonesv1.Priority, a, b *agonesv1.GameServer) int {
	before := a.CompareCountAndListPriorities(priorities, b)
	switch {
	case before == nil:
		return 0
	case *before:
		return -1
	}
	return 1
}
