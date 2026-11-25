package gameserverallocations

import (
	"math/rand"
	"slices"
	"testing"

	"agones.dev/agones/pkg/apis"
	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	allocationv1 "agones.dev/agones/pkg/apis/allocation/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ponyo877/scrim/pkg/gameservers"
)

var (
	role          = map[string]string{"role": "gameserver"}
	rolePreferred = map[string]string{"role": "gameserver", "preferred": "true"}
)

// neverPick is the pick for strategies that must not leave anything to chance.
func neverPick(t *testing.T) func(int) int {
	return func(int) int {
		t.Helper()
		require.FailNow(t, "pick must not be asked")
		return 0
	}
}

// useUp allocates from servers until nothing matches, the way the caller
// does: every GameServer found is replaced by its Allocated copy. It returns
// the names in the order they were found.
func useUp(gsa *allocationv1.GameServerAllocation, servers []*agonesv1.GameServer, pick func(int) int) []string {
	servers = slices.Clone(servers)
	var found []string
	for range servers {
		gs, ok := Find(gsa, servers, gameservers.CountByNode(servers), pick)
		if !ok {
			break
		}
		found = append(found, gs.Name)
		taken, _ := Apply(gsa, gs, noon)
		servers[slices.Index(servers, gs)] = taken
	}
	return found
}

func TestOnlyReadyAndAllocatedGameServersAreCandidates(t *testing.T) {
	gsa := request(t, allocationv1.GameServerAllocationSpec{})
	tests := map[string]struct {
		server *agonesv1.GameServer
		want   bool
	}{
		"Ready":                          {server("gs", ready), true},
		"Allocated":                      {server("gs", allocated), true},
		"Scheduled":                      {server("gs", scheduled), false},
		"Reserved":                       {server("gs", reserved), false},
		"Shutdown":                       {server("gs", shutdown), false},
		"Error":                          {server("gs", failed), false},
		"Ready but being deleted":        {server("gs", ready, beingDeleted), false},
		"Allocated but being deleted":    {server("gs", allocated, beingDeleted), false},
		"Ready but in another namespace": {server("gs", ready, in("elsewhere")), false},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, candidate(gsa, tc.server))
		})
	}
}

// Upstream: TestFindGameServerForAllocationPacked, and the packed half of
// TestAllocatorAllocatePriority.
func TestPackedUsesUpTheBusiestNodeFirst(t *testing.T) {
	tests := map[string]struct {
		spec    allocationv1.GameServerAllocationSpec
		servers []*agonesv1.GameServer
		want    []string
	}{
		"the node with the most Ready GameServers goes first": {
			spec: allocationv1.GameServerAllocationSpec{
				Selectors: []allocationv1.GameServerSelector{withLabels(role)},
			},
			servers: []*agonesv1.GameServer{
				server("gs1", ready, on("node1"), labelled(role)),
				server("gs2", ready, on("node2"), labelled(role)),
				server("gs3", ready, on("node1"), labelled(role)),
				server("gs4", ready, on("node1"), labelled(role)),
			},
			want: []string{"gs1", "gs3", "gs4", "gs2"},
		},
		"GameServers that are no candidates, or do not match, are passed over": {
			spec: allocationv1.GameServerAllocationSpec{
				Selectors: []allocationv1.GameServerSelector{withLabels(role)},
			},
			servers: []*agonesv1.GameServer{
				server("gs0", ready, on("node1"), labelled(role), beingDeleted),
				server("gs1", ready, on("node1"), labelled(role)),
				server("gs2", ready, on("node2"), labelled(role)),
				server("gs3", allocated, on("node1"), labelled(role)),
				server("gs4", allocated, on("node1"), labelled(role)),
				server("gs5", failed, on("node1"), labelled(role)),
				server("gs6", ready, on("node1"), labelled(role), in("does-not-apply")),
				server("gs7", ready, on("node1")),
			},
			want: []string{"gs1", "gs2"},
		},
		"an earlier selector wins for as long as anything matches it": {
			spec: allocationv1.GameServerAllocationSpec{
				Selectors: []allocationv1.GameServerSelector{
					withLabels(map[string]string{"preferred": "true"}),
					withLabels(role),
				},
			},
			servers: []*agonesv1.GameServer{
				server("gs1", ready, on("node1"), labelled(rolePreferred)),
				server("gs2", ready, on("node2"), labelled(role)),
				server("gs3", ready, on("node1"), labelled(role)),
				server("gs4", ready, on("node2"), labelled(rolePreferred)),
				server("gs5", ready, on("node1"), labelled(role)),
				server("gs6", ready, on("node1"), labelled(role)),
			},
			want: []string{"gs1", "gs4", "gs3", "gs5", "gs6", "gs2"},
		},
		"Allocated GameServers sort first but do not hide the Ready ones": {
			spec: allocationv1.GameServerAllocationSpec{
				Selectors: []allocationv1.GameServerSelector{withLabels(role)},
			},
			servers: []*agonesv1.GameServer{
				server("gs1", allocated, on("node1"), labelled(role)),
				server("gs2", allocated, on("node1"), labelled(role)),
				server("gs3", ready, on("node2"), labelled(role)),
				server("gs4", ready, on("node2"), labelled(role)),
			},
			want: []string{"gs3", "gs4"},
		},
		"a request without selectors takes any Ready GameServer": {
			servers: []*agonesv1.GameServer{
				server("gs1", ready, on("node1"), labelled(role)),
			},
			want: []string{"gs1"},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			gsa := request(t, tc.spec)
			require.Equal(t, apis.Packed, gsa.Spec.Scheduling, "Packed is the default")

			assert.Equal(t, tc.want, useUp(gsa, tc.servers, neverPick(t)))
		})
	}
}

func TestASelectorForAllocatedFindsAGameServerThatIsInUse(t *testing.T) {
	gsa := request(t, allocationv1.GameServerAllocationSpec{
		Selectors: []allocationv1.GameServerSelector{inState(allocated)},
	})
	servers := []*agonesv1.GameServer{
		server("gs1", ready, on("node1")),
		server("gs2", allocated, on("node2")),
		server("gs3", allocated, on("node1")),
		server("gs4", allocated, on("node1")),
	}

	gs, ok := Find(gsa, servers, gameservers.CountByNode(servers), neverPick(t))

	require.True(t, ok)
	assert.Equal(t, "gs3", gs.Name, "node1 holds the most Allocated GameServers")
	assert.Same(t, servers[2], gs, "Find returns one of the GameServers it was given")
}

// Found by TestFindReturnsAMatchOfTheFirstSelectorThatHasOne. The counts
// say on which nodes anybody has seen a Ready or Allocated GameServer. One
// on a node they do not know, or on no node at all, used to be offered
// last, as upstream does, and was allocated when nothing else matched.
func TestAGameServerOnANodeThatWasNotCountedIsPassedOver(t *testing.T) {
	counts := map[string]gameservers.NodeCount{"n1": {Ready: 1}}
	last := func(n int) int { return n - 1 }
	for _, scheduling := range []apis.SchedulingStrategy{apis.Packed, apis.Distributed} {
		t.Run(string(scheduling), func(t *testing.T) {
			gsa := request(t, allocationv1.GameServerAllocationSpec{Scheduling: scheduling})
			counted := server("a-counted", ready, on("n1"))
			strays := []*agonesv1.GameServer{server("b-uncounted", ready, on("n2")), server("c-nowhere", ready)}

			gs, found := Find(gsa, append(strays, counted), counts, last)
			require.True(t, found)
			assert.Same(t, counted, gs)

			_, found = Find(gsa, strays, counts, last)
			assert.False(t, found, "nobody is sent to a node that was not counted")
		})
	}
}

func TestFindReportsWhenNothingMatches(t *testing.T) {
	gsa := request(t, allocationv1.GameServerAllocationSpec{
		Selectors: []allocationv1.GameServerSelector{withLabels(role)},
	})
	tests := map[string][]*agonesv1.GameServer{
		"no GameServers at all":          nil,
		"none with the labels":           {server("gs1", ready)},
		"only an Allocated one":          {server("gs1", allocated, labelled(role))},
		"only one that is being deleted": {server("gs1", ready, labelled(role), beingDeleted)},
		"only one in another namespace":  {server("gs1", ready, labelled(role), in("elsewhere"))},
	}
	for name, servers := range tests {
		t.Run(name, func(t *testing.T) {
			gs, ok := Find(gsa, servers, gameservers.CountByNode(servers), neverPick(t))

			assert.False(t, ok)
			assert.Nil(t, gs)
		})
	}
}

// Upstream: TestFindGameServerForAllocationDistributed, which only checks
// that ten tries do not all give the same GameServer.
func TestDistributedLetsPickChooseAmongTheMatches(t *testing.T) {
	gsa := request(t, allocationv1.GameServerAllocationSpec{
		Selectors:  []allocationv1.GameServerSelector{withLabels(role)},
		Scheduling: apis.Distributed,
	})
	servers := []*agonesv1.GameServer{
		server("gs7", ready, on("node3"), labelled(role)),
		server("gs1", ready, on("node1"), labelled(role)),
		server("gs2", ready, on("node1"), labelled(role)),
		server("gs3", ready, on("node1"), labelled(role)),
		server("gs4", failed, on("node1"), labelled(role)),
		server("gs5", ready, on("node2"), labelled(role)),
		server("gs6", ready, on("node2"), labelled(role)),
		server("gs8", ready, on("node3")),
	}
	tests := map[string]struct {
		index int
		want  string
	}{
		"the first match by name": {index: 0, want: "gs1"},
		"one in the middle":       {index: 3, want: "gs5"},
		"the last match by name":  {index: 5, want: "gs7"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			asked := 0
			pick := func(n int) int {
				asked = n
				return tc.index
			}

			gs, ok := Find(gsa, servers, gameservers.CountByNode(servers), pick)

			require.True(t, ok)
			assert.Equal(t, tc.want, gs.Name)
			assert.Equal(t, 6, asked, "pick chooses among the matches only")
		})
	}
}

// Upstream: the distributed half of TestAllocatorAllocatePriority.
func TestDistributedUsesUpEveryMatch(t *testing.T) {
	gsa := request(t, allocationv1.GameServerAllocationSpec{
		Selectors:  []allocationv1.GameServerSelector{withLabels(role)},
		Scheduling: apis.Distributed,
	})
	servers := []*agonesv1.GameServer{
		server("gs1", ready, on("node1"), labelled(role)),
		server("gs2", ready, on("node2"), labelled(role)),
		server("gs3", ready, on("node1"), labelled(role)),
		server("gs4", ready, on("node1"), labelled(role)),
	}
	last := func(n int) int { return n - 1 }

	assert.Equal(t, []string{"gs4", "gs3", "gs2", "gs1"}, useUp(gsa, servers, last))
}

// Upstream: TestAllocatorRunLocalAllocationsCountsAndLists.
func TestPrioritiesDecideWhichGameServerGoesFirst(t *testing.T) {
	servers := []*agonesv1.GameServer{
		server("gs1", ready, on("node1"), counter("foo", 0, 1000)),                                      // 1000 free
		server("gs2", ready, on("node1"), counter("foo", 100, 1000)),                                    // 900 free
		server("gs3", ready, on("node1"), counter("foo", 999, 1000)),                                    // 1 free
		server("gs4", ready, on("node1"), list("foo", 10)),                                              // 10 free
		server("gs5", ready, on("node1"), list("foo", 10, "1")),                                         // 9 free
		server("gs6", ready, on("node1"), list("foo", 10, "1", "2", "3", "4", "5", "6", "7", "8", "9")), // 1 free
	}
	tests := map[string]struct {
		scheduling apis.SchedulingStrategy
		priorities []agonesv1.Priority
		want       []string
	}{
		"Packed, Counter Descending": {
			scheduling: apis.Packed,
			priorities: byCounter("foo", descending),
			want:       []string{"gs1", "gs2", "gs3", "gs4", "gs5", "gs6"},
		},
		"Packed, Counter Ascending": {
			scheduling: apis.Packed,
			priorities: byCounter("foo", ascending),
			want:       []string{"gs3", "gs2", "gs1", "gs4", "gs5", "gs6"},
		},
		"Distributed, Counter Descending": {
			scheduling: apis.Distributed,
			priorities: byCounter("foo", descending),
			want:       []string{"gs1", "gs2", "gs3", "gs4", "gs5", "gs6"},
		},
		"Packed, List Descending": {
			scheduling: apis.Packed,
			priorities: byList("foo", descending),
			want:       []string{"gs4", "gs5", "gs6", "gs1", "gs2", "gs3"},
		},
		"Packed, List Ascending": {
			scheduling: apis.Packed,
			priorities: byList("foo", ascending),
			want:       []string{"gs6", "gs5", "gs4", "gs1", "gs2", "gs3"},
		},
		"Distributed, List Descending": {
			scheduling: apis.Distributed,
			priorities: byList("foo", descending),
			want:       []string{"gs4", "gs5", "gs6", "gs1", "gs2", "gs3"},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			gsa := request(t, allocationv1.GameServerAllocationSpec{
				Selectors:  []allocationv1.GameServerSelector{inState(ready)},
				Scheduling: tc.scheduling,
				Priorities: tc.priorities,
			})

			assert.Equal(t, tc.want, useUp(gsa, servers, neverPick(t)))
		})
	}
}

// The store hands GameServers over in no particular order. With the same
// pick, the same request must get the same GameServer all the same.
func TestFindDoesNotDependOnTheOrderOfTheGameServers(t *testing.T) {
	random := rand.New(rand.NewSource(20))
	middle := func(n int) int { return n / 2 }
	var requests []*allocationv1.GameServerAllocation
	for _, scheduling := range []apis.SchedulingStrategy{apis.Packed, apis.Distributed} {
		for _, priorities := range [][]agonesv1.Priority{nil, byCounter("rooms", ascending), byCounter("rooms", descending)} {
			requests = append(requests, request(t, allocationv1.GameServerAllocationSpec{
				Selectors:  []allocationv1.GameServerSelector{inState(allocated), inState(ready)},
				Scheduling: scheduling,
				Priorities: priorities,
			}))
		}
	}
	for range 500 {
		servers := crowd(random)
		counts := gameservers.CountByNode(servers)
		gsa := requests[random.Intn(len(requests))]
		want, found := Find(gsa, servers, counts, middle)

		gs, ok := Find(gsa, shuffled(random, servers), counts, middle)

		require.Equal(t, found, ok)
		require.Same(t, want, gs, "%s, with priorities %v", gsa.Spec.Scheduling, gsa.Spec.Priorities)
	}
}
