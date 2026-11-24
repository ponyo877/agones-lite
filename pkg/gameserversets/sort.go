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
// The result does not depend on the order of servers: the store hands them
// over in no particular order, and what goes must not follow from that.
// Where nothing else tells two GameServers apart, the name does. Upstream
// leaves such pairs as the sort finds them.
//
// Upstream: SortGameServersByStrategy in pkg/gameserversets/gameserversets.go.
func SortForRemoval(strategy apis.SchedulingStrategy, servers []*agonesv1.GameServer, counts map[string]gameservers.NodeCount, priorities []agonesv1.Priority) []*agonesv1.GameServer {
	priorities = ascendingByDefault(priorities)
	before := oldestFirst(priorities)
	if strategy == apis.Packed {
		before = emptiestNodeFirst(counts, priorities)
	}
	sorted := slices.Clone(servers)
	sort.Slice(sorted, func(i, j int) bool { return before(sorted[i], sorted[j]) })
	return sorted
}

// ascendingByDefault returns the priorities with every order that is not
// Descending spelled out as Ascending. Upstream's CRD fills in an order that
// was left out; nothing does that here. CompareCountAndListPriorities reads
// such an order as Ascending between two GameServers that both have the key,
// and as no order at all when one of them lacks it, and a sort by that can go
// round in a circle.
func ascendingByDefault(priorities []agonesv1.Priority) []agonesv1.Priority {
	out := slices.Clone(priorities)
	for i := range out {
		if out[i].Order != agonesv1.GameServerPriorityDescending {
			out[i].Order = agonesv1.GameServerPriorityAscending
		}
	}
	return out
}

// order reports whether a is removed before b.
type order func(a, b *agonesv1.GameServer) bool

// emptiestNodeFirst is the order for Packed: scaling down should free whole
// nodes, so GameServers on the least full nodes go first.
//
// Upstream: sortGameServersByPackedStrategy.
func emptiestNodeFirst(counts map[string]gameservers.NodeCount, priorities []agonesv1.Priority) order {
	return func(a, b *agonesv1.GameServer) bool {
		countA, knownA := counts[a.Status.NodeName]
		countB, knownB := counts[b.Status.NodeName]
		fullA, fullB := countA.Ready+countA.Allocated, countB.Ready+countB.Allocated
		switch {
		case knownA != knownB:
			// A GameServer that is on no node yet, or whose node is gone,
			// goes first.
			return !knownA
		case !knownA, a.Status.NodeName == b.Status.NodeName:
			// Two such GameServers are ordered like two that share a node.
			// Upstream puts each of them before the other.
			return beforeOnSameNode(a, b, priorities)
		case fullA != fullB:
			return fullA < fullB
		}
		// Equally full nodes go by name, so that one is emptied before the
		// other is touched.
		return a.Status.NodeName < b.Status.NodeName
	}
}

// beforeOnSameNode orders two GameServers that their nodes do not tell
// apart. One that is not Ready yet goes first; then the priorities decide,
// then the name.
//
// Upstream puts one that is not Ready yet before a Ready one only, and
// leaves it to priorities and name against any other state. That can go
// round in a circle: Starting before Ready, Ready before Error, Error
// before Starting.
func beforeOnSameNode(a, b *agonesv1.GameServer, priorities []agonesv1.Priority) bool {
	if a.IsBeforeReady() != b.IsBeforeReady() {
		return a.IsBeforeReady()
	}
	if first := a.CompareCountAndListPriorities(priorities, b); first != nil {
		return *first
	}
	return a.Name < b.Name
}

// oldestFirst is the order for Distributed: the priorities decide, then age,
// then the name.
//
// Upstream: sortGameServersByDistributedStrategy. Its comment says newest
// first, but its code puts the oldest first, and this follows the code.
func oldestFirst(priorities []agonesv1.Priority) order {
	return func(a, b *agonesv1.GameServer) bool {
		if first := a.CompareCountAndListPriorities(priorities, b); first != nil {
			return *first
		}
		if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
			return a.CreationTimestamp.Before(&b.CreationTimestamp)
		}
		return a.Name < b.Name
	}
}
