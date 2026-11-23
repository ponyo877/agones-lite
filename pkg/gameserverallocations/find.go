package gameserverallocations

import (
	"slices"

	"agones.dev/agones/pkg/apis"
	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	allocationv1 "agones.dev/agones/pkg/apis/allocation/v1"

	"github.com/ponyo877/agones-lite/pkg/gameservers"
)

// Find returns the GameServer that gsa should get, or false when none matches.
//
// Selectors are tried in order: a later one is only used when nothing matches
// the ones before it. Among the matches of a selector, the one that sorts
// first wins. Distributed without priorities has nothing to sort by, so pick
// chooses instead: it is given the number of matches n and returns an index
// in [0,n).
//
// gsa must have been through ApplyDefaults, Validate and Converter. A
// selector that would not pass Validate makes Matches panic.
//
// counts says how full each node is, counting the GameServers of every
// namespace, as gameservers.CountByNode does. A GameServer on a node that
// counts does not know is passed over, whatever the strategy: nobody has
// counted a Ready or Allocated GameServer there, so the two arguments
// disagree about it, and a player is not sent on a guess.
//
// Upstream: findGameServerForAllocation in pkg/gameserverallocations/find.go.
// It shuffles the candidates and takes the first match, which makes every
// match equally likely, as a uniform pick does. A GameServer on a node its
// counter does not know it offers last (ListSortedGameServers).
func Find(gsa *allocationv1.GameServerAllocation, servers []*agonesv1.GameServer, counts map[string]gameservers.NodeCount, pick func(n int) int) (*agonesv1.GameServer, bool) {
	list := filter(servers, func(gs *agonesv1.GameServer) bool {
		_, counted := counts[gs.Status.NodeName]
		return counted && candidate(gsa, gs)
	})
	slices.SortFunc(list, orderOf(gsa, counts))
	choose := first
	if random(gsa) {
		choose = pick
	}
	for i := range gsa.Spec.Selectors {
		if matches := filter(list, gsa.Spec.Selectors[i].Matches); len(matches) > 0 {
			return matches[choose(len(matches))], true
		}
	}
	return nil, false
}

// candidate reports whether gsa may be given gs at all, whatever its
// selectors say. Allocated GameServers are candidates too: a selector can ask
// for that state, to put another session on a server that is already in use.
//
// Upstream: the GameServers that AllocationCache keeps
// (pkg/gameserverallocations/allocation_cache.go), and the namespace check in
// findGameServerForAllocation.
func candidate(gsa *allocationv1.GameServerAllocation, gs *agonesv1.GameServer) bool {
	state := gs.Status.State
	return gs.Namespace == gsa.Namespace &&
		gs.DeletionTimestamp.IsZero() &&
		(state == agonesv1.GameServerStateReady || state == agonesv1.GameServerStateAllocated)
}

// random reports whether chance decides. Distributed spreads allocations by
// choosing at random, unless priorities say which GameServer goes first.
func random(gsa *allocationv1.GameServerAllocation) bool {
	return gsa.Spec.Scheduling == apis.Distributed && len(gsa.Spec.Priorities) == 0
}

// first is the choice when the matches are sorted: the best one.
func first(int) int { return 0 }

func filter(list []*agonesv1.GameServer, keep func(*agonesv1.GameServer) bool) []*agonesv1.GameServer {
	var kept []*agonesv1.GameServer
	for _, gs := range list {
		if keep(gs) {
			kept = append(kept, gs)
		}
	}
	return kept
}
