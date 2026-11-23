package gameserverallocations

import (
	"math"
	"testing"
	"time"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	allocationv1 "agones.dev/agones/pkg/apis/allocation/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ponyo877/agones-lite/pkg/gameservers"
)

const (
	lastAllocated = gameservers.LastAllocatedAnnotation
	atNoon        = "2026-11-23T12:00:00Z"
)

func increment(amount int64) allocationv1.CounterAction {
	action := agonesv1.GameServerPriorityIncrement
	return allocationv1.CounterAction{Action: &action, Amount: &amount}
}

func capacity(n int64) *int64 { return &n }

// Upstream: TestAllocatorApplyAllocationToGameServer.
func TestApplyMarksTheGameServerAllocated(t *testing.T) {
	tests := map[string]struct {
		server          *agonesv1.GameServer
		patch           allocationv1.MetaPatch
		wantLabels      map[string]string
		wantAnnotations map[string]string
	}{
		"a request without metadata only adds the time": {
			server:          server("gs", ready),
			wantAnnotations: map[string]string{lastAllocated: atNoon},
		},
		"labels": {
			server:          server("gs", ready),
			patch:           allocationv1.MetaPatch{Labels: map[string]string{"foo": "bar"}},
			wantLabels:      map[string]string{"foo": "bar"},
			wantAnnotations: map[string]string{lastAllocated: atNoon},
		},
		"labels and annotations": {
			server: server("gs", ready),
			patch: allocationv1.MetaPatch{
				Labels:      map[string]string{"foo": "bar"},
				Annotations: map[string]string{"bar": "foo"},
			},
			wantLabels:      map[string]string{"foo": "bar"},
			wantAnnotations: map[string]string{"bar": "foo", lastAllocated: atNoon},
		},
		"the request's labels are added to the GameServer's own, and win": {
			server:          server("gs", ready, labelled(map[string]string{"mode": "lobby", "fleet": "blue"})),
			patch:           allocationv1.MetaPatch{Labels: map[string]string{"mode": "deathmatch"}},
			wantLabels:      map[string]string{"mode": "deathmatch", "fleet": "blue"},
			wantAnnotations: map[string]string{lastAllocated: atNoon},
		},
		"the request cannot choose the time itself": {
			server:          server("gs", ready),
			patch:           allocationv1.MetaPatch{Annotations: map[string]string{lastAllocated: "never"}},
			wantAnnotations: map[string]string{lastAllocated: atNoon},
		},
		"a GameServer that is allocated again gets a new time": {
			server: server("gs", allocated, func(gs *agonesv1.GameServer) {
				gs.Annotations = map[string]string{lastAllocated: "2026-11-23T09:00:00Z"}
			}),
			wantAnnotations: map[string]string{lastAllocated: atNoon},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			gsa := request(t, allocationv1.GameServerAllocationSpec{MetaPatch: tc.patch})

			gs, errs := Apply(gsa, tc.server, noon)

			assert.Empty(t, errs)
			assert.Equal(t, allocated, gs.Status.State)
			assert.Equal(t, tc.wantLabels, gs.Labels)
			assert.Equal(t, tc.wantAnnotations, gs.Annotations)
		})
	}
}

func TestApplyWritesTheTimeInUTC(t *testing.T) {
	gsa := request(t, allocationv1.GameServerAllocationSpec{})
	tokyo := time.FixedZone("JST", 9*60*60)
	halfPastNine := time.Date(2026, 11, 23, 21, 30, 0, 500_000_000, tokyo)

	gs, _ := Apply(gsa, server("gs", ready), halfPastNine)

	assert.Equal(t, "2026-11-23T12:30:00.5Z", gs.Annotations[lastAllocated])
	var parsed time.Time
	require.NoError(t, parsed.UnmarshalText([]byte(gs.Annotations[lastAllocated])))
	assert.True(t, parsed.Equal(halfPastNine))
}

// Upstream: TestAllocatorApplyAllocationToGameServerCountsListsActions.
func TestApplyRunsTheCounterAndListActions(t *testing.T) {
	tests := map[string]struct {
		server       *agonesv1.GameServer
		counters     map[string]allocationv1.CounterAction
		lists        map[string]allocationv1.ListAction
		wantCounters map[string]agonesv1.CounterStatus
		wantLists    map[string]agonesv1.ListStatus
	}{
		"a Counter is incremented, a List gets a new capacity and loses and gains values": {
			server: server("gs1", ready,
				counter("rooms", 101, 1000),
				list("players", 100, "alice", "bob", "cat")),
			counters: map[string]allocationv1.CounterAction{"rooms": increment(1)},
			lists: map[string]allocationv1.ListAction{"players": {
				AddValues:    []string{"x7un", "8inz"},
				Capacity:     capacity(40),
				DeleteValues: []string{"bob"},
			}},
			wantCounters: map[string]agonesv1.CounterStatus{"rooms": {Count: 102, Capacity: 1000}},
			wantLists:    map[string]agonesv1.ListStatus{"players": {Values: []string{"alice", "cat", "x7un", "8inz"}, Capacity: 40}},
		},
		"a Counter stops at its capacity, and so does a List": {
			server: server("gs2", ready,
				counter("rooms", 101, 1000),
				list("players", 100)),
			counters: map[string]allocationv1.CounterAction{"rooms": increment(1000)},
			lists: map[string]allocationv1.ListAction{"players": {
				AddValues: []string{"x7un", "8inz"},
				Capacity:  capacity(1),
			}},
			wantCounters: map[string]agonesv1.CounterStatus{"rooms": {Count: 1000, Capacity: 1000}},
			wantLists:    map[string]agonesv1.ListStatus{"players": {Values: []string{"x7un"}, Capacity: 1}},
		},
		// Found by TestApplyMovesACountTheWayTheRequestSays. Upstream adds
		// the two as int64; the sum is below zero, and it makes that 0.
		"a Counter given more than an int64 has room for is full, not empty": {
			server:       server("gs4", ready, counter("rooms", 1, 3)),
			counters:     map[string]allocationv1.CounterAction{"rooms": increment(math.MaxInt64)},
			wantCounters: map[string]agonesv1.CounterStatus{"rooms": {Count: 3, Capacity: 3}},
		},
		"a List may be given the largest capacity upstream allows by default": {
			server:    server("gs3", ready, list("players", 100)),
			lists:     map[string]allocationv1.ListAction{"players": {Capacity: capacity(1000)}},
			wantLists: map[string]agonesv1.ListStatus{"players": {Capacity: 1000}},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			gsa := request(t, allocationv1.GameServerAllocationSpec{Counters: tc.counters, Lists: tc.lists})

			gs, errs := Apply(gsa, tc.server, noon)

			assert.Empty(t, errs)
			assert.Equal(t, allocated, gs.Status.State)
			assert.Equal(t, tc.wantCounters, gs.Status.Counters)
			assert.Equal(t, tc.wantLists, gs.Status.Lists)
		})
	}
}

func TestApplyStillAllocatesWhenActionsFail(t *testing.T) {
	gsa := request(t, allocationv1.GameServerAllocationSpec{
		Counters: map[string]allocationv1.CounterAction{
			"rooms":   increment(1),
			"missing": increment(1),
		},
		Lists: map[string]allocationv1.ListAction{
			"players": {Capacity: capacity(1001), AddValues: []string{"bob"}},
			"nowhere": {AddValues: []string{"carol"}},
		},
	})
	before := server("gs", ready, counter("rooms", 1, 10), list("players", 10, "alice"))

	gs, errs := Apply(gsa, before, noon)

	assert.Equal(t, allocated, gs.Status.State)
	assert.Equal(t, agonesv1.CounterStatus{Count: 2, Capacity: 10}, gs.Status.Counters["rooms"])
	assert.Equal(t, agonesv1.ListStatus{Values: []string{"alice", "bob"}, Capacity: 10}, gs.Status.Lists["players"],
		"the capacity was refused, the new value was not")
	require.Len(t, errs, 3, "counters first, then lists, each by name")
	assert.ErrorContains(t, errs[0], "Counter not found")
	assert.ErrorContains(t, errs[1], "Name nowhere")
	assert.ErrorContains(t, errs[2], "Capacity must be between 0 and 1000")
}

func TestApplyLeavesItsArgumentsAlone(t *testing.T) {
	gsa := request(t, allocationv1.GameServerAllocationSpec{
		MetaPatch: allocationv1.MetaPatch{
			Labels:      map[string]string{"mode": "deathmatch"},
			Annotations: map[string]string{"map": "searide"},
		},
		Counters: map[string]allocationv1.CounterAction{"rooms": increment(1)},
		Lists:    map[string]allocationv1.ListAction{"players": {AddValues: []string{"bob"}}},
	})
	gs := server("gs", ready,
		labelled(map[string]string{"mode": "lobby"}),
		counter("rooms", 1, 10),
		list("players", 10, "alice"))
	gsaBefore, gsBefore := gsa.DeepCopy(), gs.DeepCopy()

	Apply(gsa, gs, noon)

	assert.Equal(t, gsaBefore, gsa)
	assert.Equal(t, gsBefore, gs)
}
