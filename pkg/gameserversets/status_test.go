package gameserversets

import (
	"math"
	"testing"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	"github.com/stretchr/testify/assert"
)

// The first four cases are upstream's "compute status" in TestComputeStatus.
func TestStatusCountsGameServersByState(t *testing.T) {
	tests := map[string]struct {
		servers []*agonesv1.GameServer
		want    agonesv1.GameServerSetStatus
	}{
		"no GameServers": {},
		"Ready ones are counted apart": {
			servers: of(creating, ready),
			want:    agonesv1.GameServerSetStatus{Replicas: 2, ReadyReplicas: 1},
		},
		"so are Allocated ones": {
			servers: of(allocated, allocated, creating, ready),
			want:    agonesv1.GameServerSetStatus{Replicas: 4, ReadyReplicas: 1, AllocatedReplicas: 2},
		},
		"and Reserved ones": {
			servers: of(reserved, reserved, ready),
			want:    agonesv1.GameServerSetStatus{Replicas: 3, ReadyReplicas: 1, ReservedReplicas: 2},
		},
		"GameServers on their way out are not replicas any more": {
			servers: []*agonesv1.GameServer{server("", ready), server("", ready, beingDeleted), server("", shutdown)},
			want:    agonesv1.GameServerSetStatus{Replicas: 1, ReadyReplicas: 1, ShutdownReplicas: 2},
		},
		"broken GameServers are replicas until they are removed": {
			servers: of(unhealthy, failed),
			want:    agonesv1.GameServerSetStatus{Replicas: 2},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			// The status always has both maps, even when they are empty.
			tc.want.Counters = map[string]agonesv1.AggregatedCounterStatus{}
			tc.want.Lists = map[string]agonesv1.AggregatedListStatus{}

			assert.Equal(t, tc.want, Status(setOf(10), tc.servers))
		})
	}
}

// Upstream: "counters" and "counters with no gameservers" in TestComputeStatus.
func TestStatusAddsUpCounters(t *testing.T) {
	const full = math.MaxInt64
	tests := map[string]struct {
		template map[string]agonesv1.CounterStatus
		servers  []*agonesv1.GameServer
		want     map[string]agonesv1.AggregatedCounterStatus
	}{
		"every GameServer adds to the total, Allocated ones also to their own": {
			servers: []*agonesv1.GameServer{
				server("", allocated, counter("firstCounter", 5, 10), counter("secondCounter", 100, 1000), counter("fullCounter", full, full)),
				server("", reserved, counter("firstCounter", 10, 15), counter("fullCounter", full, full)),
				server("", creating, counter("firstCounter", 20, 30), counter("secondCounter", 100, 1000), counter("fullCounter", full, full)),
				server("", ready, counter("firstCounter", 15, 30), counter("secondCounter", 20, 200), counter("fullCounter", full, full)),
			},
			want: map[string]agonesv1.AggregatedCounterStatus{
				"firstCounter":  {AllocatedCount: 5, AllocatedCapacity: 10, Count: 50, Capacity: 85},
				"secondCounter": {AllocatedCount: 100, AllocatedCapacity: 1000, Count: 220, Capacity: 2200},
				"fullCounter":   {AllocatedCount: full, AllocatedCapacity: full, Count: full, Capacity: full},
			},
		},
		"a Counter in the template is reported before any GameServer has it": {
			template: map[string]agonesv1.CounterStatus{
				"firstCounter":  {Count: 1, Capacity: 10},
				"secondCounter": {Count: 1, Capacity: 10},
			},
			want: map[string]agonesv1.AggregatedCounterStatus{"firstCounter": {}, "secondCounter": {}},
		},
		"a GameServer on its way out adds nothing": {
			servers: []*agonesv1.GameServer{
				server("", ready, counter("players", 3, 10)),
				server("", shutdown, counter("players", 4, 10)),
				server("", allocated, beingDeleted, counter("players", 5, 10)),
			},
			want: map[string]agonesv1.AggregatedCounterStatus{"players": {Count: 3, Capacity: 10}},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			set := setOf(10)
			set.Spec.Template.Spec.Counters = tc.template

			assert.Equal(t, tc.want, Status(set, tc.servers).Counters)
		})
	}
}

// Upstream: "lists" and "lists with no gameservers" in TestComputeStatus.
func TestStatusAddsUpLists(t *testing.T) {
	tests := map[string]struct {
		template map[string]agonesv1.ListStatus
		servers  []*agonesv1.GameServer
		want     map[string]agonesv1.AggregatedListStatus
	}{
		"every GameServer adds to the total, Allocated ones also to their own": {
			servers: []*agonesv1.GameServer{
				server("", allocated, list("firstList", 10, "a", "b"), list("secondList", 1000, "1", "2")),
				server("", reserved, list("firstList", 15, "c")),
				server("", creating, list("firstList", 30, "d"), list("secondList", 1000, "3")),
				server("", ready, list("firstList", 30), list("secondList", 100, "4", "5", "6")),
			},
			want: map[string]agonesv1.AggregatedListStatus{
				"firstList":  {AllocatedCount: 2, AllocatedCapacity: 10, Count: 4, Capacity: 85},
				"secondList": {AllocatedCount: 2, AllocatedCapacity: 1000, Count: 6, Capacity: 2100},
			},
		},
		"a List in the template is reported before any GameServer has it": {
			template: map[string]agonesv1.ListStatus{
				"firstList":  {Capacity: 10, Values: []string{"a", "b"}},
				"secondList": {Capacity: 1000, Values: []string{"1", "2"}},
			},
			want: map[string]agonesv1.AggregatedListStatus{"firstList": {}, "secondList": {}},
		},
		"a value on two GameServers is counted twice": {
			servers: []*agonesv1.GameServer{
				server("", ready, list("rooms", 5, "lobby")),
				server("", ready, list("rooms", 5, "lobby")),
			},
			want: map[string]agonesv1.AggregatedListStatus{"rooms": {Count: 2, Capacity: 10}},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			set := setOf(10)
			set.Spec.Template.Spec.Lists = tc.template

			assert.Equal(t, tc.want, Status(set, tc.servers).Lists)
		})
	}
}
