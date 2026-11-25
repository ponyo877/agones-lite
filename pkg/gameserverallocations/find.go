package gameserverallocations

import (
	"cmp"
	"maps"
	"slices"

	"agones.dev/agones/pkg/apis"
	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	allocationv1 "agones.dev/agones/pkg/apis/allocation/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/ponyo877/scrim/pkg/gameservers"
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
	before := orderOf(gsa, counts)
	for i := range gsa.Spec.Selectors {
		matches := filter(list, matcher(&gsa.Spec.Selectors[i]))
		switch {
		case len(matches) == 0:
			continue
		case random(gsa):
			// Sorted only so that the same pick gives the same GameServer.
			slices.SortFunc(matches, before)
			return matches[pick(len(matches))], true
		}
		// The one that sorts first, without sorting the rest: with ten
		// thousand candidates that is most of the work saved.
		return slices.MinFunc(matches, before), true
	}
	return nil, false
}

// FindPacked is Find for the Packed strategy, for when there are many
// GameServers: it looks at the nodes first and at the GameServers of as few
// nodes as it can, where Find looks at every GameServer there is.
//
// Packed sends a session to the node that is busiest already, so the
// GameServers of the busiest node that has a match are all it has to see.
// Nodes that are equally busy are looked at together, and among their
// matches the priorities and then the name decide, as in Find.
//
// counts is as for Find, but only of the nodes players may be sent to. on
// returns the GameServers a node has in a state.
func FindPacked(gsa *allocationv1.GameServerAllocation, counts map[string]gameservers.NodeCount, on func(node string, state agonesv1.GameServerState) []*agonesv1.GameServer) (*agonesv1.GameServer, bool) {
	busiestFirst := byHowBusy(counts)
	for i := range gsa.Spec.Selectors {
		matches := matcher(&gsa.Spec.Selectors[i])
		wanted := func(gs *agonesv1.GameServer) bool { return candidate(gsa, gs) && matches(gs) }
		// A GameServer that is in use comes before any that is not: see
		// allocatedFirst.
		for _, state := range []agonesv1.GameServerState{agonesv1.GameServerStateAllocated, agonesv1.GameServerStateReady} {
			if gs, found := firstOn(busiestFirst, state, on, wanted, distributed(gsa.Spec.Priorities)); found {
				return gs, true
			}
		}
	}
	return nil, false
}

// tries is how often FindSpread draws before it leaves the request to Find.
// Where every other GameServer matches, eight draws miss one time in 256.
const tries = 8

// FindSpread is Find for the Distributed strategy without priorities, for
// when there are many GameServers: it draws the one Find would as likely
// have chosen, and does not look at the rest.
//
// Find chooses evenly among all that match. FindSpread chooses evenly among
// all there are in the state the request asks for, which the counts say
// without a look at any of them, and takes its choice if it matches. One
// that does not is drawn again. Every match is then as likely as every
// other, as with Find, and a request that most GameServers match is
// answered after a draw or two.
//
// It reports false when it found nothing: after tries draws, or for a
// request it is not for. That says nothing about whether there is a match,
// and Find has to be asked.
//
// counts is as for FindPacked. one returns a GameServer of a node in a
// state by its place among them, as gameservers.OneOnNodeIn does.
func FindSpread(gsa *allocationv1.GameServerAllocation, counts map[string]gameservers.NodeCount, one func(node string, state agonesv1.GameServerState, place int) (*agonesv1.GameServer, bool), pick func(n int) int) (*agonesv1.GameServer, bool) {
	if !random(gsa) || len(gsa.Spec.Selectors) == 0 {
		return nil, false
	}
	selector := &gsa.Spec.Selectors[0]
	state, matches := stateOf(selector), matcher(selector)
	nodes, total := slices.Sorted(maps.Keys(counts)), 0
	for _, node := range nodes {
		total += countIn(counts[node], state)
	}
	for range min(tries, total) {
		node, place := nodeOf(pick(total), nodes, counts, state)
		if gs, there := one(node, state, place); there && candidate(gsa, gs) && matches(gs) {
			return gs, true
		}
	}
	return nil, false
}

// nodeOf turns a place among all the GameServers in a state into the node
// that has it and its place on that node: the nodes are gone through in
// order, each with as many places as it counts.
func nodeOf(place int, nodes []string, counts map[string]gameservers.NodeCount, state agonesv1.GameServerState) (string, int) {
	for _, node := range nodes {
		here := countIn(counts[node], state)
		if place < here {
			return node, place
		}
		place -= here
	}
	return "", 0 // a place beyond the last: nobody has it
}

// stateOf is the state a selector asks for: Ready, unless it says Allocated.
func stateOf(selector *allocationv1.GameServerSelector) agonesv1.GameServerState {
	if selector.GameServerState != nil {
		return *selector.GameServerState
	}
	return agonesv1.GameServerStateReady
}

// countIn is how many GameServers a node has in a state, of the two that
// are counted.
func countIn(count gameservers.NodeCount, state agonesv1.GameServerState) int {
	if state == agonesv1.GameServerStateAllocated {
		return int(count.Allocated)
	}
	return int(count.Ready)
}

// firstOn returns the best of the wanted GameServers on the first group of
// nodes that has any.
func firstOn(groups [][]string, state agonesv1.GameServerState, on func(string, agonesv1.GameServerState) []*agonesv1.GameServer, wanted func(*agonesv1.GameServer) bool, before order) (*agonesv1.GameServer, bool) {
	for _, nodes := range groups {
		var found []*agonesv1.GameServer
		for _, node := range nodes {
			found = append(found, filter(on(node, state), wanted)...)
		}
		if len(found) > 0 {
			return slices.MinFunc(found, before), true
		}
	}
	return nil, false
}

// byHowBusy sorts the nodes the way Packed prefers them, the one with the
// most sessions first, and puts those that are equally busy into one group.
func byHowBusy(counts map[string]gameservers.NodeCount) [][]string {
	nodes := slices.Collect(maps.Keys(counts))
	busier := func(a, b string) int {
		return cmp.Or(cmp.Compare(counts[b].Allocated, counts[a].Allocated), cmp.Compare(counts[b].Ready, counts[a].Ready))
	}
	slices.SortFunc(nodes, busier)
	var groups [][]string
	for i, node := range nodes {
		if i == 0 || busier(nodes[i-1], node) != 0 {
			groups = append(groups, nil)
		}
		groups[len(groups)-1] = append(groups[len(groups)-1], node)
	}
	return groups
}

// matcher returns what selector.Matches says, made for asking it about many
// GameServers. The labels are compared here, with a selector that is made
// once for all of them, and Matches is left with the rest: the state, the
// Counters and the Lists.
func matcher(selector *allocationv1.GameServerSelector) func(*agonesv1.GameServer) bool {
	byLabels, err := metav1.LabelSelectorAsSelector(&selector.LabelSelector)
	if err != nil {
		return selector.Matches // it says what is wrong with the request, loudly
	}
	rest := *selector
	rest.LabelSelector = metav1.LabelSelector{}
	return func(gs *agonesv1.GameServer) bool {
		return byLabels.Matches(labels.Set(gs.Labels)) && rest.Matches(gs)
	}
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

func filter(list []*agonesv1.GameServer, keep func(*agonesv1.GameServer) bool) []*agonesv1.GameServer {
	var kept []*agonesv1.GameServer
	for _, gs := range list {
		if keep(gs) {
			kept = append(kept, gs)
		}
	}
	return kept
}
