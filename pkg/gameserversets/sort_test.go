package gameserversets

import (
	"math/rand"
	"testing"
	"time"

	"agones.dev/agones/pkg/apis"
	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ponyo877/agones-lite/pkg/gameservers"
)

func byCounter(name, order string) []agonesv1.Priority {
	return []agonesv1.Priority{{Type: agonesv1.GameServerPriorityCounter, Key: name, Order: order}}
}

// Upstream: TestSortGameServersByPackedStrategy.
func TestPackedEmptiesTheLeastFullNodesFirst(t *testing.T) {
	counts := map[string]gameservers.NodeCount{
		"n1": {Ready: 1, Allocated: 0},
		"n2": {Ready: 0, Allocated: 2},
		"n3": {Ready: 2, Allocated: 2},
		"n4": {Ready: 2, Allocated: 2},
	}
	servers := []*agonesv1.GameServer{
		server("g1", ready, on("n2")),
		server("g2", ready),
		server("g3", ready, on("n1")),
		server("g4", creating, on("n2")),
		server("g5", ready, on("n3")),
		server("g6", ready, on("n4")),
		server("g7", ready, on("n3")),
		server("g8", ready, on("n4")),
		server("g9", allocated, on("n3"), counter("foo", 0, 100)),
		server("g10", allocated, on("n3"), counter("foo", 99, 100)),
		server("g11", allocated, on("n4"), counter("foo", 0, 90)),
		server("g12", allocated, on("n4"), counter("foo", 89, 90)),
	}
	tests := map[string]struct {
		priorities []agonesv1.Priority
		want       []string
	}{
		"within a node, Descending starts with the most room in the Counter": {
			priorities: byCounter("foo", agonesv1.GameServerPriorityDescending),
			want:       []string{"g2", "g3", "g4", "g1", "g9", "g10", "g5", "g7", "g11", "g12", "g6", "g8"},
		},
		"within a node, Ascending starts with the least room in the Counter": {
			priorities: byCounter("foo", agonesv1.GameServerPriorityAscending),
			want:       []string{"g2", "g3", "g4", "g1", "g10", "g9", "g5", "g7", "g12", "g11", "g6", "g8"},
		},
		"without priorities, the name decides within a node": {
			want: []string{"g2", "g3", "g4", "g1", "g10", "g5", "g7", "g9", "g11", "g12", "g6", "g8"},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			sorted := SortForRemoval(apis.Packed, servers, counts, tc.priorities)

			assert.Equal(t, tc.want, names(sorted))
		})
	}
}

// Upstream: TestSortGameServersByDistributedStrategy.
func TestDistributedRemovesTheOldestFirst(t *testing.T) {
	g1 := server("g1", ready, created(noon.Add(10*time.Second)))
	g2 := server("g2", ready, created(noon))
	g3 := server("g3", ready, created(noon.Add(30*time.Second)))
	g4 := server("g4", ready, created(noon.Add(30*time.Second)), counter("bar", 0, 100))
	g5 := server("g5", ready, created(noon), counter("bar", 0, 100))
	g6 := server("g6", ready, created(noon), counter("bar", 0, 1000))

	tests := map[string]struct {
		servers    []*agonesv1.GameServer
		priorities []agonesv1.Priority
		want       []string
	}{
		"without priorities, age alone decides": {
			servers: []*agonesv1.GameServer{g1, g2, g3},
			want:    []string{"g2", "g1", "g3"},
		},
		"Descending starts with the most room, then the oldest": {
			servers:    []*agonesv1.GameServer{g4, g6, g5},
			priorities: byCounter("bar", agonesv1.GameServerPriorityDescending),
			want:       []string{"g6", "g5", "g4"},
		},
		"Ascending starts with the least room, then the oldest": {
			servers:    []*agonesv1.GameServer{g4, g5, g6},
			priorities: byCounter("bar", agonesv1.GameServerPriorityAscending),
			want:       []string{"g5", "g4", "g6"},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			sorted := SortForRemoval(apis.Distributed, tc.servers, nil, tc.priorities)

			assert.Equal(t, tc.want, names(sorted))
		})
	}
}

func TestAStrategyThatIsNotPackedSortsAsDistributed(t *testing.T) {
	servers := []*agonesv1.GameServer{
		server("young", ready, on("empty"), created(noon)),
		server("old", ready, on("full"), created(noon.Add(-time.Hour))),
	}
	counts := map[string]gameservers.NodeCount{"empty": {Ready: 1}, "full": {Ready: 9}}

	assert.Equal(t, []string{"young", "old"}, names(SortForRemoval(apis.Packed, servers, counts, nil)))
	assert.Equal(t, []string{"old", "young"}, names(SortForRemoval("", servers, counts, nil)))
}

func TestSortForRemovalLeavesItsArgumentAlone(t *testing.T) {
	servers := []*agonesv1.GameServer{
		server("young", ready, created(noon)),
		server("old", ready, created(noon.Add(-time.Hour))),
	}

	sorted := SortForRemoval(apis.Distributed, servers, nil, nil)

	assert.Equal(t, []string{"old", "young"}, names(sorted))
	assert.Equal(t, []string{"young", "old"}, names(servers))
}

// Upstream leaves each of these to the order its cache lists them in.
func TestRemovalOrderWhereUpstreamHasNone(t *testing.T) {
	counts := map[string]gameservers.NodeCount{"n1": {Ready: 2}}
	tests := map[string]struct {
		strategy apis.SchedulingStrategy
		servers  []*agonesv1.GameServer
		want     []string
	}{
		"Distributed: GameServers of the same age go by name": {
			strategy: apis.Distributed,
			servers:  []*agonesv1.GameServer{server("b", ready, created(noon)), server("a", ready, created(noon))},
			want:     []string{"a", "b"},
		},
		"Packed: GameServers on no node, or on one that is gone, go by name": {
			strategy: apis.Packed,
			servers:  []*agonesv1.GameServer{server("c", ready, on("n1")), server("b", ready), server("a", ready, on("gone"))},
			want:     []string{"a", "b", "c"},
		},
		// By name alone, "ready" would go before "stuck", "stuck" before
		// "unready", and "unready", not being Ready yet, before "ready".
		"Packed: one that is not Ready yet goes before every other state on its node": {
			strategy: apis.Packed,
			servers:  []*agonesv1.GameServer{server("stuck", failed, on("n1")), server("ready", ready, on("n1")), server("unready", creating, on("n1"))},
			want:     []string{"unready", "ready", "stuck"},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			sorted := SortForRemoval(tc.strategy, tc.servers, counts, nil)

			assert.Equal(t, tc.want, names(sorted))
		})
	}
}

// Found by TestSortForRemovalGivesTheSameOrderForAnyOrderOfTheGameServers.
// Admission lets a Fleet leave the order of a priority out. Upstream then
// compares two GameServers that both have the Counter as Ascending, and
// leaves a pair of which one lacks it to the name: "c-full" before "a-roomy"
// by the Counter, "a-roomy" before "b-bare" and "b-bare" before "c-full" by
// name. Sorting such a circle gave three different answers for six orders
// of the same three GameServers.
func TestAPriorityWithoutAnOrderIsAscendingWhateverTheOrderOfTheInput(t *testing.T) {
	roomy := server("a-roomy", ready, counter("rooms", 1, 3))
	bare := server("b-bare", ready)
	full := server("c-full", ready, counter("rooms", 2, 3))
	counts := map[string]gameservers.NodeCount{}
	orders := [][]*agonesv1.GameServer{
		{roomy, bare, full}, {roomy, full, bare}, {bare, roomy, full},
		{bare, full, roomy}, {full, roomy, bare}, {full, bare, roomy},
	}
	for _, strategy := range []apis.SchedulingStrategy{apis.Packed, apis.Distributed} {
		for _, servers := range orders {
			sorted := SortForRemoval(strategy, servers, counts, byCounter("rooms", ""))

			assert.Equal(t, []string{"c-full", "a-roomy", "b-bare"}, names(sorted), "%s, given %v", strategy, names(servers))
		}
	}
}

// The store hands GameServers over in no particular order, so which of them
// go first must not depend on it.
func TestSortForRemovalDoesNotDependOnTheOrderOfItsInput(t *testing.T) {
	random := rand.New(rand.NewSource(20))
	for range 500 {
		servers, priorities := crowd(random), anyPriorities(random)
		for _, strategy := range []apis.SchedulingStrategy{apis.Packed, apis.Distributed} {
			want := names(SortForRemoval(strategy, servers, crowdedNodes, priorities))

			got := names(SortForRemoval(strategy, shuffled(random, servers), crowdedNodes, priorities))

			require.Equal(t, want, got, "%s, with priorities %v", strategy, priorities)
		}
	}
}
