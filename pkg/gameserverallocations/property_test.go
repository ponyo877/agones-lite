package gameserverallocations

import (
	"fmt"
	"maps"
	"math"
	"slices"
	"testing"

	"agones.dev/agones/pkg/apis"
	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	allocationv1 "agones.dev/agones/pkg/apis/allocation/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"pgregory.net/rapid"

	"github.com/ponyo877/agones-lite/pkg/agones"
	"github.com/ponyo877/agones-lite/pkg/gameservers"
)

// These tests state what must hold for any request and any GameServers and
// let rapid look for a pair that breaks it. A failure is printed as the
// smallest such pair.

// aCrowd draws up to eight GameServers that tie in every way an allocation
// looks at: a few states, nodes, labels and amounts of room. Some are in
// another namespace, on no node, or being deleted. Only the names all differ.
func aCrowd(t *rapid.T) []*agonesv1.GameServer {
	servers := make([]*agonesv1.GameServer, rapid.IntRange(0, 8).Draw(t, "servers"))
	for i := range servers {
		servers[i] = aGameServer(t, fmt.Sprintf("gs-%d", i))
	}
	return servers
}

func aGameServer(t *rapid.T, name string) *agonesv1.GameServer {
	states := []agonesv1.GameServerState{ready, ready, allocated, allocated, reserved, scheduled, shutdown}
	gs := server(name, rapid.SampledFrom(states).Draw(t, "state"),
		on(rapid.SampledFrom([]string{"n1", "n2", "n3", ""}).Draw(t, "node")),
		in(rapid.SampledFrom([]string{namespace, namespace, namespace, "elsewhere"}).Draw(t, "namespace")),
		labelled(map[string]string{"mode": rapid.SampledFrom([]string{"duel", "squad"}).Draw(t, "mode")}))
	maybe(t, "rooms", gs, counter("rooms", rapid.Int64Range(0, 3).Draw(t, "roomsTaken"), 3))
	maybe(t, "players", gs, list("players", 3, []string{"ann", "bob", "cy"}[:rapid.IntRange(0, 3).Draw(t, "playersIn")]...))
	if rapid.IntRange(0, 7).Draw(t, "beingDeleted") == 0 {
		beingDeleted(gs)
	}
	return gs
}

// maybe applies opt to gs when rapid says so.
func maybe(t *rapid.T, label string, gs *agonesv1.GameServer, opt option) {
	if rapid.Bool().Draw(t, label) {
		opt(gs)
	}
}

// someCounts counts the GameServers by node, as the caller of Find does,
// and now and then forgets a node.
func someCounts(t *rapid.T, servers []*agonesv1.GameServer) map[string]gameservers.NodeCount {
	counts := gameservers.CountByNode(servers)
	if rapid.Bool().Draw(t, "forgetsANode") {
		delete(counts, rapid.SampledFrom([]string{"n1", "n2", "n3"}).Draw(t, "forgotten"))
	}
	return counts
}

// aRequest draws a request the way the caller prepares one: with defaults,
// valid, and with its selectors in one list.
func aRequest(t *rapid.T) *allocationv1.GameServerAllocation {
	require.NoError(t, agones.Setup())
	gsa := &allocationv1.GameServerAllocation{ObjectMeta: metav1.ObjectMeta{Namespace: namespace}}
	gsa.Spec.Scheduling = rapid.SampledFrom([]apis.SchedulingStrategy{apis.Packed, apis.Distributed}).Draw(t, "scheduling")
	gsa.Spec.Selectors = rapid.SliceOfN(rapid.Custom(aSelector), 0, 3).Draw(t, "selectors")
	gsa.Spec.Priorities = rapid.SampledFrom([][]agonesv1.Priority{
		nil, byCounter("rooms", ascending), byCounter("rooms", descending), byList("players", ""),
	}).Draw(t, "priorities")
	gsa.Spec.Priorities = slices.Clone(gsa.Spec.Priorities)
	gsa.Spec.Counters = map[string]allocationv1.CounterAction{"rooms": aCounterAction(t)}
	gsa.Spec.Lists = map[string]allocationv1.ListAction{"players": aListAction(t)}
	gsa.Spec.MetaPatch.Labels = map[string]string{"mode": "taken"}
	gsa.ApplyDefaults()
	require.Empty(t, gsa.Validate())
	gsa.Converter()
	return gsa
}

func aSelector(t *rapid.T) allocationv1.GameServerSelector {
	var selector allocationv1.GameServerSelector
	if mode := rapid.SampledFrom([]string{"", "duel", "squad"}).Draw(t, "mode"); mode != "" {
		selector = withLabels(map[string]string{"mode": mode})
	}
	if state := rapid.SampledFrom([]agonesv1.GameServerState{"", ready, allocated}).Draw(t, "state"); state != "" {
		selector.GameServerState = &state
	}
	if rapid.Bool().Draw(t, "byRooms") {
		selector.Counters = map[string]allocationv1.CounterSelector{"rooms": {MinAvailable: rapid.Int64Range(0, 3).Draw(t, "minAvailable")}}
	}
	if rapid.Bool().Draw(t, "byPlayers") {
		selector.Lists = map[string]allocationv1.ListSelector{"players": {ContainsValue: rapid.SampledFrom([]string{"", "ann"}).Draw(t, "contains")}}
	}
	return selector
}

// anAmount is how much a request may move a Counter by, or resize it to:
// a little, nothing, or as much as an int64 holds.
func anAmount() *rapid.Generator[*int64] {
	return rapid.Ptr(rapid.OneOf(rapid.Int64Range(0, 5), rapid.Just(int64(math.MaxInt64))), true)
}

func aCounterAction(t *rapid.T) allocationv1.CounterAction {
	action := allocationv1.CounterAction{Capacity: anAmount().Draw(t, "capacity")}
	if amount := anAmount().Draw(t, "amount"); amount != nil {
		direction := rapid.SampledFrom([]string{agonesv1.GameServerPriorityIncrement, agonesv1.GameServerPriorityDecrement}).Draw(t, "action")
		action.Action, action.Amount = &direction, amount
	}
	return action
}

func aListAction(t *rapid.T) allocationv1.ListAction {
	names := rapid.SliceOfN(rapid.SampledFrom([]string{"ann", "bob", "cy", "dee", "eve"}), 0, 4)
	return allocationv1.ListAction{
		Capacity:     anAmount().Draw(t, "listCapacity"),
		AddValues:    names.Draw(t, "add"),
		DeleteValues: names.Draw(t, "delete"),
	}
}

// middle is a pick that depends on nothing but the number of matches.
func middle(n int) int { return n / 2 }

// matchesOf returns the GameServers that Find may hand out for a selector:
// the candidates on a node that was counted, as far as the selector accepts
// them. It asks every question about every GameServer, in no clever order.
func matchesOf(gsa *allocationv1.GameServerAllocation, selector allocationv1.GameServerSelector, servers []*agonesv1.GameServer, counts map[string]gameservers.NodeCount) []*agonesv1.GameServer {
	var matches []*agonesv1.GameServer
	for _, gs := range servers {
		_, counted := counts[gs.Status.NodeName]
		playable := gs.Status.State == ready || gs.Status.State == allocated
		if counted && playable && gs.Namespace == gsa.Namespace && gs.DeletionTimestamp == nil && selector.Matches(gs) {
			matches = append(matches, gs)
		}
	}
	return matches
}

// firstMatches returns the matches of the first selector that has any.
func firstMatches(gsa *allocationv1.GameServerAllocation, servers []*agonesv1.GameServer, counts map[string]gameservers.NodeCount) []*agonesv1.GameServer {
	for _, selector := range gsa.Spec.Selectors {
		if matches := matchesOf(gsa, selector, servers, counts); len(matches) > 0 {
			return matches
		}
	}
	return nil
}

// Find asks a selector about every candidate through matcher, which
// compares the labels itself and leaves the rest to Matches. The two must
// say the same of any GameServer: by its labels, its state, its Counters
// and its Lists.
func TestMatcherSaysWhatMatchesSays(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		gsa := aRequest(t)
		servers := aCrowd(t)
		for i := range gsa.Spec.Selectors {
			selector := &gsa.Spec.Selectors[i]
			quick := matcher(selector)
			for _, gs := range servers {
				require.Equal(t, selector.Matches(gs), quick(gs), "selector %d about %s", i, gs.Name)
			}
		}
	})
}

// A player is sent to whatever Find returns. So it must be a GameServer
// the request asked for: one that the first selector with any match
// accepts, that is up, in the request's namespace, not on its way out, and
// on a node the caller has counted GameServers on. And when there is such
// a GameServer, Find must not come back empty-handed.
func TestFindReturnsAMatchOfTheFirstSelectorThatHasOne(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		gsa, servers := aRequest(t), aCrowd(t)
		counts := someCounts(t, servers)
		want := firstMatches(gsa, servers, counts)

		gs, found := Find(gsa, servers, counts, middle)

		if len(want) == 0 {
			require.False(t, found, "nothing matches, but %v was found", gs)
			return
		}
		require.True(t, found, "%v match", names(want))
		assert.Contains(t, want, gs, "%s is no match of the first selector that has one", gs.Name)
	})
}

// Packed fills the busiest nodes first, so that the quiet ones can empty
// out and be removed. A match on a node with more sessions than the chosen
// one's node would have been the better choice.
func TestPackedFindsNoMatchOnANodeWithMoreSessionsThanTheChosenOnes(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		gsa, servers := aRequest(t), aCrowd(t)
		gsa.Spec.Scheduling = apis.Packed
		counts := someCounts(t, servers)

		gs, found := Find(gsa, servers, counts, middle)

		if !found {
			return
		}
		chosen := counts[gs.Status.NodeName]
		for _, other := range firstMatches(gsa, servers, counts) {
			if other.Status.State != gs.Status.State {
				continue // upstream offers Allocated GameServers before Ready ones, whatever their nodes
			}
			busier := counts[other.Status.NodeName]
			assert.LessOrEqual(t, busier.Allocated, chosen.Allocated, "%s on %s was passed over for %s on %s", other.Name, other.Status.NodeName, gs.Name, gs.Status.NodeName)
		}
	})
}

// The store hands GameServers over in no particular order. With the same
// pick, the same request must get the same GameServer all the same, or two
// allocators would disagree about who is next.
func TestFindGivesTheSameAnswerForAnyOrderOfTheGameServers(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		gsa, servers := aRequest(t), aCrowd(t)
		counts := someCounts(t, servers)
		want, wantFound := Find(gsa, servers, counts, middle)

		gs, found := Find(gsa, rapid.Permutation(servers).Draw(t, "shuffled"), counts, middle)

		assert.Equal(t, wantFound, found)
		assert.Same(t, want, gs)
	})
}

// What Find and Apply are given is the store's own view of the world, and
// is shared with everything else that reads the store. They must only
// look.
func TestFindAndApplyLeaveWhatTheyAreGivenAlone(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		gsa, servers := aRequest(t), aCrowd(t)
		counts := someCounts(t, servers)
		request, crowd, numbers := gsa.DeepCopy(), copies(servers), maps.Clone(counts)

		if gs, found := Find(gsa, servers, counts, middle); found {
			Apply(gsa, gs, noon)
		}

		assert.Equal(t, request, gsa, "the request")
		assert.Equal(t, crowd, servers, "the GameServers, and their order")
		assert.Equal(t, numbers, counts, "the counts")
	})
}

func copies(servers []*agonesv1.GameServer) []*agonesv1.GameServer {
	out := make([]*agonesv1.GameServer, len(servers))
	for i, gs := range servers {
		out[i] = gs.DeepCopy()
	}
	return out
}

// An allocation may fill a Counter, empty it, resize it, and add players
// to a List or take them off. Whatever it asks for, what is stored must
// still be a Counter and a List: a count from zero to the capacity, and
// distinct values, no more than the capacity. The sidecar, the selectors
// and the autoscaler all take that for granted.
func TestApplyLeavesEveryCounterAndListInOrder(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		gsa, gs := aRequest(t), aGameServer(t, "gs")

		taken, _ := Apply(gsa, gs, noon)

		assert.Equal(t, allocated, taken.Status.State)
		for name, counter := range taken.Status.Counters {
			assert.True(t, 0 <= counter.Count && counter.Count <= counter.Capacity, "%s holds %d of %d", name, counter.Count, counter.Capacity)
		}
		for name, list := range taken.Status.Lists {
			assert.LessOrEqual(t, int64(len(list.Values)), list.Capacity, "%s holds %v", name, list.Values)
			assert.LessOrEqual(t, list.Capacity, int64(maxListCapacity), name)
			assert.Len(t, slices.Compact(slices.Sorted(slices.Values(list.Values))), len(list.Values), "%s holds a value twice: %v", name, list.Values)
		}
	})
}

// Adding to a count must not make it smaller, nor taking away larger:
// with the count upside down, a full GameServer looks empty and gets every
// next session as well.
func TestApplyMovesACountTheWayTheRequestSays(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		gsa, gs := aRequest(t), aGameServer(t, "gs")
		action := gsa.Spec.Counters["rooms"]
		action.Capacity = nil // a new capacity may cut the count, which is another matter
		gsa.Spec.Counters["rooms"] = action
		before, has := gs.Status.Counters["rooms"]

		taken, _ := Apply(gsa, gs, noon)

		after := taken.Status.Counters["rooms"]
		switch {
		case !has || action.Action == nil:
			assert.Equal(t, before, after)
		case *action.Action == agonesv1.GameServerPriorityIncrement:
			assert.Equal(t, least(before.Capacity-before.Count, *action.Amount), after.Count-before.Count, "added")
		default:
			assert.Equal(t, least(before.Count, *action.Amount), before.Count-after.Count, "taken away")
		}
	})
}

func least(a, b int64) int64 { return min(a, b) }

// FindPacked is the quick way to what Find finds for Packed: it looks at
// the GameServers node by node, and at no more nodes than it must. Given
// the same GameServers, sorted by node and state as the store has them,
// the two must find the same one.
func TestFindPackedFindsWhatFindFinds(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		gsa := aRequest(t)
		gsa.Spec.Scheduling = apis.Packed
		servers := aCrowd(t)
		counts := someCounts(t, servers)
		on := func(node string, state agonesv1.GameServerState) []*agonesv1.GameServer {
			return filter(servers, func(gs *agonesv1.GameServer) bool {
				return gs.Status.NodeName == node && gs.Status.State == state
			})
		}
		want, wantFound := Find(gsa, servers, counts, neverAsked(t))

		gs, found := FindPacked(gsa, counts, on)

		require.Equal(t, wantFound, found)
		require.Same(t, want, gs)
	})
}

// neverAsked is the pick for a strategy that leaves nothing to chance.
func neverAsked(t *rapid.T) func(int) int {
	return func(int) int {
		t.Fatalf("pick must not be asked")
		return 0
	}
}
