package gameserverallocations

import (
	"slices"
	"testing"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	"github.com/stretchr/testify/assert"

	"github.com/ponyo877/scrim/pkg/gameservers"
)

// sortedBy returns the names of servers in the given order.
func sortedBy(o order, servers []*agonesv1.GameServer) []string {
	list := slices.Clone(servers)
	slices.SortFunc(list, o)
	return names(list)
}

// Upstream: TestAllocationCacheListSortedGameServers.
func TestPackedOffersTheBusiestNodesFirst(t *testing.T) {
	gs1 := server("gs1", ready, on("node1"))
	gs2 := server("gs2", ready, on("node2"))
	gs3 := server("gs3", allocated, on("node1"))
	gs4 := server("gs4", ready, on("node2"))
	gs5 := server("gs5", ready, on("node1"), counter("players", 4, 40))  // 36 free
	gs6 := server("gs6", ready, on("node1"), counter("players", 14, 40)) // 26 free
	gs7 := server("gs7", allocated, on("node3"))
	gs8 := server("gs8", ready, on("node3"))

	tests := map[string]struct {
		servers    []*agonesv1.GameServer
		priorities []agonesv1.Priority
		want       []string
	}{
		"Allocated GameServers come first": {
			servers: []*agonesv1.GameServer{gs1, gs2, gs3},
			want:    []string{"gs3", "gs1", "gs2"},
		},
		"then the node with the most Allocated, even when another has more Ready": {
			servers: []*agonesv1.GameServer{gs2, gs4, gs8, gs7},
			want:    []string{"gs7", "gs8", "gs2", "gs4"},
		},
		"then the node with the most Ready": {
			servers: []*agonesv1.GameServer{gs1, gs2, gs4},
			want:    []string{"gs2", "gs4", "gs1"},
		},
		"then the name": {
			servers: []*agonesv1.GameServer{gs2, gs1},
			want:    []string{"gs1", "gs2"},
		},
		"Descending priorities break a tie between nodes with the most room first": {
			servers:    []*agonesv1.GameServer{gs1, gs2, gs3, gs4, gs5, gs6},
			priorities: byCounter("players", descending),
			want:       []string{"gs3", "gs5", "gs6", "gs1", "gs2", "gs4"},
		},
		"Ascending priorities break it with the least room first": {
			servers:    []*agonesv1.GameServer{gs1, gs2, gs3, gs4, gs5, gs6},
			priorities: byCounter("players", ascending),
			want:       []string{"gs3", "gs6", "gs5", "gs1", "gs2", "gs4"},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			counts := gameservers.CountByNode(tc.servers)

			assert.Equal(t, tc.want, sortedBy(packed(tc.priorities, counts), tc.servers))
		})
	}
}

func TestPackedRanksAnUnknownNodeAsEmpty(t *testing.T) {
	servers := []*agonesv1.GameServer{
		server("gs1", ready, on("unknown-b")),
		server("gs2", ready, on("unknown-a")),
		server("gs3", ready, on("known")),
	}
	counts := map[string]gameservers.NodeCount{"known": {Ready: 1}}

	assert.Equal(t, []string{"gs3", "gs1", "gs2"}, sortedBy(packed(nil, counts), servers))
}

// Upstream: TestListSortedGameServersPriorities.
func TestDistributedSortsByPrioritiesAndThenByName(t *testing.T) {
	gs1 := server("gs1", ready,
		list("players", 100, "player1"),                   // 99 free
		list("layers", 100, "layer1", "layer2", "layer3")) // 97 free
	gs2 := server("gs2", ready,
		list("players", 100),         // 100 free
		counter("assets", 101, 1000)) // 899 free
	gs3 := server("gs3", ready,
		list("players", 100, "player2", "player3"), // 98 free
		counter("sessions", 9, 1000),               // 991 free
		counter("assets", 100, 1000))               // 900 free
	gs4 := server("gs4", ready,
		counter("sessions", 99, 1000),           // 901 free
		list("players", 100, "player4"),         // 99 free
		list("layers", 100, "layer4", "layer5")) // 98 free
	gs5 := server("gs5", ready,
		counter("sessions", 9, 1000), // 991 free
		counter("assets", 99, 1000),  // 901 free
		list("layers", 100))          // 100 free
	gs6 := server("gs6", ready,
		counter("sessions", 999, 1000)) // 1 free

	tests := map[string]struct {
		servers    []*agonesv1.GameServer
		priorities []agonesv1.Priority
		want       []string
	}{
		"one Counter, Ascending": {
			servers:    []*agonesv1.GameServer{gs4, gs5, gs6},
			priorities: byCounter("sessions", ascending),
			want:       []string{"gs6", "gs4", "gs5"},
		},
		"one Counter, Descending": {
			servers:    []*agonesv1.GameServer{gs4, gs5, gs6},
			priorities: byCounter("sessions", descending),
			want:       []string{"gs5", "gs4", "gs6"},
		},
		"two Counters, Ascending and Ascending": {
			servers:    []*agonesv1.GameServer{gs3, gs5, gs6, gs4, gs1, gs2},
			priorities: append(byCounter("sessions", ascending), byCounter("assets", ascending)...),
			want:       []string{"gs6", "gs4", "gs3", "gs5", "gs2", "gs1"},
		},
		"two Counters, Ascending and Descending": {
			servers:    []*agonesv1.GameServer{gs3, gs5, gs6, gs4, gs1, gs2},
			priorities: append(byCounter("sessions", ascending), byCounter("assets", descending)...),
			want:       []string{"gs6", "gs4", "gs5", "gs3", "gs2", "gs1"},
		},
		"a GameServer without the Counter comes last": {
			servers:    []*agonesv1.GameServer{gs1, gs5, gs6, gs4},
			priorities: byCounter("sessions", ascending),
			want:       []string{"gs6", "gs4", "gs5", "gs1"},
		},
		"one List, Ascending": {
			servers:    []*agonesv1.GameServer{gs3, gs2, gs1},
			priorities: byList("players", ascending),
			want:       []string{"gs3", "gs1", "gs2"},
		},
		"one List, Descending": {
			servers:    []*agonesv1.GameServer{gs3, gs2, gs1},
			priorities: byList("players", descending),
			want:       []string{"gs2", "gs1", "gs3"},
		},
		"two Lists, Descending and Ascending": {
			servers:    []*agonesv1.GameServer{gs1, gs2, gs3, gs4},
			priorities: append(byList("players", descending), byList("layers", ascending)...),
			want:       []string{"gs2", "gs1", "gs4", "gs3"},
		},
		"two Lists, Descending and Descending": {
			servers:    []*agonesv1.GameServer{gs6, gs5, gs4, gs3, gs2, gs1},
			priorities: append(byList("players", descending), byList("layers", descending)...),
			want:       []string{"gs2", "gs4", "gs1", "gs3", "gs5", "gs6"},
		},
		"the name decides when no GameServer has the priority": {
			servers:    []*agonesv1.GameServer{gs6, gs5, gs4, gs3, gs2, gs1},
			priorities: byCounter("sayers", ascending),
			want:       []string{"gs1", "gs2", "gs3", "gs4", "gs5", "gs6"},
		},
		"the name decides when there are no priorities": {
			servers: []*agonesv1.GameServer{gs6, gs5, gs4, gs3, gs2, gs1},
			want:    []string{"gs1", "gs2", "gs3", "gs4", "gs5", "gs6"},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, sortedBy(distributed(tc.priorities), tc.servers))
		})
	}
}
