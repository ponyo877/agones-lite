package fleetautoscalers

import (
	"math"
	"math/rand"
	"slices"
	"testing"

	"agones.dev/agones/pkg/apis"
	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	autoscalingv1 "agones.dev/agones/pkg/apis/autoscaling/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ponyo877/scrim/pkg/gameservers"
)

const (
	mostRoomFirst  = agonesv1.GameServerPriorityDescending
	leastRoomFirst = agonesv1.GameServerPriorityAscending
)

// capacityCase is a Fleet with a Counter or a List, and what a policy makes
// of it.
type capacityCase struct {
	perReplica usage  // the Counter or List in the Fleet's template
	replicas   int32  // the Fleet's status.replicas
	total      usage  // the Counter or List in the Fleet's status
	allocated  int64  // the part of total.count on Allocated GameServers
	order      string // which GameServers scaling down takes first
	policy     autoscalingv1.FleetAutoscalerPolicy
	servers    []*agonesv1.GameServer
	want       int32
	limited    bool
}

func counterFleet(tc capacityCase) *agonesv1.Fleet {
	return fleetOf(removing(agonesv1.GameServerPriorityCounter, rooms, tc.order), func(fleet *agonesv1.Fleet) {
		fleet.Status.Replicas = tc.replicas
		fleet.Spec.Template.Spec.Counters = map[string]agonesv1.CounterStatus{
			rooms: {Count: tc.perReplica.count, Capacity: tc.perReplica.capacity},
		}
		fleet.Status.Counters = map[string]agonesv1.AggregatedCounterStatus{
			rooms: {Count: tc.total.count, Capacity: tc.total.capacity, AllocatedCount: tc.allocated},
		}
	})
}

func listFleet(tc capacityCase) *agonesv1.Fleet {
	return fleetOf(removing(agonesv1.GameServerPriorityList, players, tc.order), func(fleet *agonesv1.Fleet) {
		fleet.Status.Replicas = tc.replicas
		fleet.Spec.Template.Spec.Lists = map[string]agonesv1.ListStatus{
			players: {Values: values(tc.perReplica.count), Capacity: tc.perReplica.capacity},
		}
		fleet.Status.Lists = map[string]agonesv1.AggregatedListStatus{
			players: {Count: tc.total.count, Capacity: tc.total.capacity, AllocatedCount: tc.allocated},
		}
	})
}

// Upstream: TestApplyCounterPolicy.
func TestCounterKeepsCapacityFree(t *testing.T) {
	counts := map[string]gameservers.NodeCount{"n1": {Ready: 1, Allocated: 1}}
	tests := map[string]capacityCase{
		"an empty Fleet grows until it has the minimum capacity": {
			perReplica: usage{0, 7}, replicas: 0, total: usage{0, 0},
			policy: counterPolicy(amount(10), 10, 100),
			want:   2, limited: true,
		},
		"too little free capacity adds the replicas that make up for it": {
			perReplica: usage{0, 7}, replicas: 10, total: usage{68, 70},
			policy: counterPolicy(amount(10), 10, 100),
			want:   12,
		},
		"a new replica only adds the capacity it does not use itself": {
			perReplica: usage{7, 10}, replicas: 3, total: usage{21, 30},
			policy: counterPolicy(amount(25), 10, 100),
			want:   9,
		},
		"free capacity equal to the buffer changes nothing": {
			perReplica: usage{0, 7}, replicas: 10, total: usage{60, 70},
			policy: counterPolicy(amount(10), 10, 100),
			want:   10,
		},
		"growing stops at the maximum capacity": {
			perReplica: usage{0, 7}, replicas: 14, total: usage{98, 98},
			policy: counterPolicy(amount(10), 10, 100),
			want:   14, limited: true,
		},
		"capacity below the minimum grows to it, however much is free": {
			perReplica: usage{0, 7}, replicas: 7, total: usage{1, 49},
			policy: counterPolicy(amount(10), 70, 700),
			want:   10, limited: true,
		},
		"capacity at the minimum stays, however much is free": {
			perReplica: usage{0, 7}, replicas: 10, total: usage{1, 70},
			policy: counterPolicy(amount(10), 70, 700),
			want:   10, limited: true,
		},
		"too much free capacity removes GameServers for as long as the buffer stays free": {
			perReplica: usage{0, 7}, replicas: 8, total: usage{31, 70}, order: leastRoomFirst,
			policy:  counterPolicy(amount(10), 10, 100),
			servers: readyOn("n1", counting, usage{10, 10}, usage{3, 5}, usage{7, 7}, usage{11, 14}, usage{0, 13}, usage{0, 7}, usage{0, 7}, usage{0, 7}),
			want:    1,
		},
		"removing stops before it cuts into the buffer": {
			perReplica: usage{7, 10}, replicas: 3, total: usage{21, 30},
			policy:  counterPolicy(amount(5), 10, 100),
			servers: readyOn("n1", counting, usage{7, 10}, usage{7, 10}, usage{7, 10}),
			want:    2,
		},
		"removing stops when exactly the buffer is left": {
			perReplica: usage{0, 5}, replicas: 3, total: usage{0, 15}, order: mostRoomFirst,
			policy:  counterPolicy(amount(5), 1, 5),
			servers: readyOn("n1", counting, usage{0, 5}, usage{0, 5}, usage{0, 5}),
			want:    1,
		},
		"removing stops before the capacity falls below the minimum": {
			perReplica: usage{0, 7}, replicas: 4, total: usage{1, 36}, order: mostRoomFirst,
			policy:  counterPolicy(amount(1), 7, 700),
			servers: readyOn("n1", counting, usage{0, 10}, usage{1, 5}, usage{0, 7}, usage{0, 14}),
			want:    2, limited: true,
		},
		"capacity above the maximum comes down to it, but never to no replicas": {
			perReplica: usage{0, 7}, replicas: 1, total: usage{7, 7}, order: mostRoomFirst,
			policy:  counterPolicy(amount(1), 0, 2),
			servers: readyOn("n1", counting, usage{7, 7}),
			want:    1, limited: true,
		},
		"a buffer in percent is a share of the capacity to come": {
			// 30% free means that the 8 Allocated are the other 70%.
			perReplica: usage{0, 1}, replicas: 10, total: usage{8, 10}, allocated: 8,
			policy: counterPolicy(share("30%"), 10, 100),
			want:   12,
		},
		"a buffer in percent is filled by what new replicas leave unused": {
			perReplica: usage{3, 10}, replicas: 3, total: usage{20, 30}, allocated: 20,
			policy: counterPolicy(share("50%"), 10, 100),
			want:   5,
		},
		"a share of nothing still grows to the minimum capacity": {
			perReplica: usage{0, 10}, replicas: 3, total: usage{20, 30}, allocated: 0,
			policy: counterPolicy(share("10%"), 50, 100),
			want:   5, limited: true,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			replicas, limited, applied, err := Desired(tc.policy, counterFleet(tc), tc.servers, counts, noon, neverCalled(t))

			require.NoError(t, err)
			assert.Equal(t, tc.want, replicas)
			assert.Equal(t, tc.limited, limited)
			assert.Equal(t, counterType, applied)
		})
	}
}

// Upstream: TestApplyListPolicy.
func TestListKeepsCapacityFree(t *testing.T) {
	counts := map[string]gameservers.NodeCount{"n1": {Allocated: 2}, "n2": {Ready: 1}}
	tests := map[string]capacityCase{
		"an empty Fleet grows until it has the minimum capacity": {
			perReplica: usage{0, 7}, replicas: 0, total: usage{0, 0},
			policy: listPolicy(amount(10), 10, 100),
			want:   2, limited: true,
		},
		"too little free capacity adds the replicas that make up for it": {
			perReplica: usage{2, 3}, replicas: 10, total: usage{29, 30},
			policy: listPolicy(amount(5), 10, 100),
			want:   14,
		},
		"growing takes the replicas that still fit under the maximum capacity": {
			perReplica: usage{3, 5}, replicas: 3, total: usage{9, 15},
			policy: listPolicy(amount(15), 15, 25),
			want:   5, limited: true,
		},
		"nothing is added when the capacity is at the maximum already": {
			perReplica: usage{2, 3}, replicas: 10, total: usage{29, 30},
			policy: listPolicy(amount(5), 10, 30),
			want:   10, limited: true,
		},
		"too much free capacity removes GameServers for as long as the buffer stays free": {
			perReplica: usage{1, 10}, replicas: 8, total: usage{15, 70}, order: mostRoomFirst,
			policy:  listPolicy(amount(10), 10, 70),
			servers: readyOn("n1", listing, usage{0, 10}, usage{0, 10}, usage{0, 10}, usage{8, 8}, usage{4, 10}, usage{1, 4}, usage{1, 8}, usage{1, 10}),
			want:    4,
		},
		"removing stops when the capacity is down to the minimum": {
			perReplica: usage{0, 5}, replicas: 4, total: usage{3, 20}, order: leastRoomFirst,
			policy:  listPolicy(amount(1), 10, 100),
			servers: readyOn("n1", listing, usage{0, 5}, usage{0, 5}, usage{0, 5}, usage{3, 5}),
			want:    2, limited: true,
		},
		"a buffer in percent is a share of the capacity to come": {
			perReplica: usage{1, 3}, replicas: 11, total: usage{30, 30}, allocated: 29,
			policy: listPolicy(share("10%"), 10, 50),
			want:   13,
		},
		"a buffer in percent stops growing at the maximum capacity too": {
			perReplica: usage{3, 10}, replicas: 3, total: usage{20, 30}, allocated: 20,
			policy: listPolicy(share("50%"), 10, 45),
			want:   4, limited: true,
		},
		"a buffer in percent removes the GameServers of the emptiest node first": {
			perReplica: usage{2, 10}, replicas: 5, total: usage{18, 50}, allocated: 15, order: mostRoomFirst,
			policy: listPolicy(share("50%"), 0, 50),
			servers: slices.Concat(
				readyOn("n1", listing, usage{5, 15}, usage{7, 10}, usage{3, 5}),
				readyOn("n2", listing, usage{3, 5}, usage{0, 15}),
			),
			want: 3,
		},
		"a share of nothing leaves one replica when no minimum asks for more": {
			perReplica: usage{2, 10}, replicas: 3, total: usage{15, 30}, allocated: 0, order: mostRoomFirst,
			policy:  listPolicy(share("20%"), 0, 50),
			servers: readyOn("n1", listing, usage{5, 15}, usage{7, 10}, usage{3, 5}),
			want:    1, limited: true,
		},
		"a share of nothing leaves one replica when one covers the minimum": {
			perReplica: usage{2, 10}, replicas: 3, total: usage{15, 30}, allocated: 0, order: mostRoomFirst,
			policy:  listPolicy(share("20%"), 1, 50),
			servers: readyOn("n1", listing, usage{5, 15}, usage{7, 10}, usage{3, 5}),
			want:    1, limited: true,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			replicas, limited, applied, err := Desired(tc.policy, listFleet(tc), tc.servers, counts, noon, neverCalled(t))

			require.NoError(t, err)
			assert.Equal(t, tc.want, replicas)
			assert.Equal(t, tc.limited, limited)
			assert.Equal(t, listType, applied)
		})
	}
}

// Upstream: TestApplyListPolicyFlapping. One GameServer is full and
// Allocated, the others are empty and Ready, and the policy wants the
// capacity of two GameServers free.
func TestScalingDownLeavesAllocatedGameServersAlone(t *testing.T) {
	counts := map[string]gameservers.NodeCount{"n1": {Ready: 2}, "n2": {Allocated: 1}}
	policy := listPolicy(amount(120), 120, 300)
	busy := func(name string) *agonesv1.GameServer {
		return server(name, allocated, on("n2"), listing(55, 60))
	}
	idle := readyOn("n1", listing, usage{0, 60}, usage{0, 60})
	tests := map[string]capacityCase{
		"one more replica fills the buffer": {
			replicas: 2, total: usage{55, 120}, allocated: 55,
			servers: slices.Concat(idle, []*agonesv1.GameServer{busy("busy")}),
			want:    3,
		},
		"with the buffer just over full, taking a Ready one away would leave too little": {
			replicas: 3, total: usage{55, 180}, allocated: 55,
			servers: slices.Concat(idle, []*agonesv1.GameServer{busy("busy")}),
			want:    3,
		},
		"below the minimum capacity the Fleet grows": {
			replicas: 1, total: usage{55, 60}, allocated: 55,
			servers: []*agonesv1.GameServer{busy("busy")},
			want:    2, limited: true,
		},
		"above the maximum capacity nothing can go while every GameServer is Allocated": {
			replicas: 6, total: usage{330, 360}, allocated: 330,
			servers: []*agonesv1.GameServer{busy("b1"), busy("b2"), busy("b3"), busy("b4"), busy("b5"), busy("b6")},
			want:    6, limited: true,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			tc.perReplica = usage{0, 60}

			replicas, limited, _, err := Desired(policy, listFleet(tc), tc.servers, counts, noon, neverCalled(t))

			require.NoError(t, err)
			assert.Equal(t, tc.want, replicas)
			assert.Equal(t, tc.limited, limited)
		})
	}
}

func TestScalingDownTakesOnlyWhatMayBeRemoved(t *testing.T) {
	counts := map[string]gameservers.NodeCount{"n1": {Ready: 1, Allocated: 1}}
	tests := map[string]capacityCase{
		"Allocated and Reserved GameServers stay, whatever the buffer says": {
			perReplica: usage{0, 10}, replicas: 3, total: usage{0, 30},
			servers: []*agonesv1.GameServer{
				server("a", allocated, on("n1"), counting(0, 10)),
				server("b", reserved, on("n1"), counting(0, 10)),
				server("c", ready, on("n1"), counting(0, 10)),
			},
			want: 2,
		},
		// Upstream's scaleDown does this, so it is kept.
		"a GameServer without the Counter counts as removed, though it frees nothing": {
			perReplica: usage{0, 10}, replicas: 3, total: usage{0, 20},
			servers: []*agonesv1.GameServer{
				server("a", ready, on("n1")),
				server("b", ready, on("n1"), counting(0, 10)),
				server("c", ready, on("n1"), counting(0, 10)),
			},
			want: 1,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			policy := counterPolicy(amount(10), 0, 100)

			replicas, limited, _, err := Desired(policy, counterFleet(tc), tc.servers, counts, noon, neverCalled(t))

			require.NoError(t, err)
			assert.Equal(t, tc.want, replicas)
			assert.Equal(t, tc.limited, limited)
		})
	}
}

// The Fleet's status does not count a GameServer that is being deleted or
// has shut down, so its replicas and its capacity are not there to be taken
// off. Upstream takes them off all the same.
func TestScalingDownLeavesOutGameServersThatAreGoingAnyway(t *testing.T) {
	counts := map[string]gameservers.NodeCount{"n1": {Ready: 3}}
	last := server("last", ready, on("n1"), counting(0, 10))
	tests := map[string]capacityCase{
		"one that is being deleted": {
			replicas: 1, total: usage{0, 10},
			policy: counterPolicy(amount(5), 0, 100),
			servers: []*agonesv1.GameServer{
				server("gone", ready, on("n1"), counting(2, 7), goingAway()),
				last,
			},
			want: 1,
		},
		"one that has shut down": {
			replicas: 1, total: usage{0, 10},
			policy: counterPolicy(amount(5), 0, 100),
			servers: []*agonesv1.GameServer{
				server("gone", agonesv1.GameServerStateShutdown, on("n1"), counting(2, 7)),
				last,
			},
			want: 1,
		},
		"several of them, which used to take the size below zero": {
			replicas: 1, total: usage{0, 10},
			policy: counterPolicy(amount(5), 0, 100),
			servers: []*agonesv1.GameServer{
				server("gone-1", ready, on("n1"), counting(0, 1), goingAway()),
				server("gone-2", ready, on("n1"), counting(0, 1), goingAway()),
				server("gone-3", ready, on("n1"), counting(0, 1), goingAway()),
				last,
			},
			want: 1,
		},
		"an Allocated one that is being deleted": {
			replicas: 1, total: usage{0, 10},
			policy: counterPolicy(amount(5), 0, 100),
			servers: []*agonesv1.GameServer{
				server("gone", allocated, on("n1"), counting(2, 7), goingAway()),
				last,
			},
			want: 1,
		},
		"capacity above the maximum comes down by GameServers that are still there": {
			replicas: 3, total: usage{25, 30},
			policy: counterPolicy(amount(5), 0, 20),
			servers: slices.Concat(
				[]*agonesv1.GameServer{server("gone", ready, on("n1"), counting(0, 4), goingAway())},
				readyOn("n1", counting, usage{10, 10}, usage{10, 10}, usage{5, 10}),
			),
			want: 2, limited: true,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			tc.perReplica = usage{0, 10}

			replicas, limited, _, err := Desired(tc.policy, counterFleet(tc), tc.servers, counts, noon, neverCalled(t))

			require.NoError(t, err)
			assert.Equal(t, tc.want, replicas)
			assert.Equal(t, tc.limited, limited)
		})
	}
}

// Upstream holds the size at one replica only when it runs out of
// GameServers to remove, not when it stops earlier.
func TestScalingDownStopsAtOneReplicaWhateverTheStatusSays(t *testing.T) {
	counts := map[string]gameservers.NodeCount{"n1": {Ready: 3}}
	behind := capacityCase{
		// Three GameServers are there, and the status has counted one.
		perReplica: usage{0, 10}, replicas: 1, total: usage{0, 30},
		servers: readyOn("n1", counting, usage{0, 10}, usage{0, 10}, usage{0, 10}),
	}

	replicas, limited, _, err := Desired(counterPolicy(amount(10), 0, 100), counterFleet(behind), behind.servers, counts, noon, neverCalled(t))

	require.NoError(t, err)
	assert.Equal(t, fewest, replicas)
	assert.False(t, limited)
}

// Upstream converts what it works out to int32 without looking, and
// multiplies as int64 without looking either.
func TestNumbersTooLargeForAFleetDoNotWrapAround(t *testing.T) {
	const billion = 1_000_000_000
	tests := map[string]capacityCase{
		"a minimum capacity that takes more replicas than a Fleet can have": {
			perReplica: usage{0, 1}, replicas: 5, total: usage{0, 5},
			policy: counterPolicy(amount(1), 3*billion, math.MaxInt64),
			want:   math.MaxInt32, limited: true,
		},
		"a buffer that takes more replicas than a Fleet can have": {
			perReplica: usage{0, 1}, replicas: 5, total: usage{30 * billion, 30 * billion}, allocated: 30 * billion,
			policy: counterPolicy(share("50%"), 1, math.MaxInt64),
			want:   math.MaxInt32,
		},
		"a maximum capacity with room for more replicas than a Fleet can have": {
			perReplica: usage{0, 1}, replicas: 5, total: usage{30 * billion, 30 * billion}, allocated: 30 * billion,
			policy: counterPolicy(share("50%"), 1, 40*billion),
			want:   math.MaxInt32, limited: true,
		},
		"a capacity of which a few replicas more are beyond int64": {
			perReplica: usage{1<<62 - 1, 1 << 62}, replicas: 1, total: usage{1 << 62, 1 << 62},
			policy: counterPolicy(amount(4), 0, math.MaxInt64),
			want:   1, limited: true,
		},
		"an Allocated count of which a hundred times is beyond int64": {
			// Half of the capacity to come has to be free: 10 replicas more.
			perReplica: usage{0, 1e16}, replicas: 10, total: usage{1e17, 1e17}, allocated: 1e17,
			policy: counterPolicy(share("50%"), 1, math.MaxInt64),
			want:   20,
		},
		// Found by TestACounterPolicyGrowsByExactlyTheReplicasThatWholeNumbersGive.
		// One more replica is exactly the half that has to be free. Upstream
		// divides in floating point, finds one unit missing, and adds two.
		"an Allocated count with more digits than a float64 holds": {
			perReplica: usage{0, 1e15 + 6}, replicas: 1, total: usage{1e15 + 6, 1e15 + 6}, allocated: 1e15 + 6,
			policy: counterPolicy(share("50%"), 1, math.MaxInt64),
			want:   2,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			replicas, limited, _, err := Desired(tc.policy, counterFleet(tc), nil, nil, noon, neverCalled(t))

			require.NoError(t, err)
			assert.Equal(t, tc.want, replicas)
			assert.Equal(t, tc.limited, limited)
		})
	}
}

// The store hands GameServers over in no particular order. How many of them
// scaling down takes depends on which go first, so that must not follow
// from the order they came in.
func TestDesiredDoesNotDependOnTheOrderOfTheGameServers(t *testing.T) {
	random := rand.New(rand.NewSource(20))
	counts := map[string]gameservers.NodeCount{"n1": {Ready: 3}, "n2": {Ready: 3}}
	for range 500 {
		servers := crowd(random)
		fleet := counterFleet(capacityCase{
			perReplica: usage{0, 3}, replicas: int32(len(servers)), total: roomsIn(servers),
			order: []string{"", mostRoomFirst, leastRoomFirst}[random.Intn(3)],
		})
		fleet.Spec.Scheduling = []apis.SchedulingStrategy{apis.Packed, apis.Distributed}[random.Intn(2)]
		policy := counterPolicy(amount(int32(1+random.Intn(6))), 0, 100)
		want, wantLimited, _, err := Desired(policy, fleet, servers, counts, noon, neverCalled(t))
		require.NoError(t, err)

		replicas, limited, _, err := Desired(policy, fleet, shuffled(random, servers), counts, noon, neverCalled(t))

		require.NoError(t, err)
		require.Equal(t, want, replicas, "%s, removing %v", fleet.Spec.Scheduling, fleet.Spec.Priorities)
		require.Equal(t, wantLimited, limited)
	}
}

// The suspect this was written for: a share of 100% divides by zero. With
// nothing Allocated that is 0/0, which Go turns into the smallest int64 on
// amd64 and into 0 on arm64. This Fleet then came out at 2 replicas,
// limited, on the one and stayed at 3 on the other. Admission refuses the
// share, but it does not look at the policy inside a Schedule.
func TestAShareOfTheWholeFleetIsRefusedOnEveryCPU(t *testing.T) {
	tc := capacityCase{perReplica: usage{0, 4}, replicas: 3, total: usage{12, 12}}
	servers := readyOn("n1", counting, usage{4, 4}, usage{4, 4}, usage{4, 4})
	inner := counterPolicy(share("100%"), 5, 100)
	tests := map[string]autoscalingv1.FleetAutoscalerPolicy{
		"on its own":        inner,
		"inside a Schedule": schedulePolicy(between("", ""), inner),
	}
	for name, policy := range tests {
		t.Run(name, func(t *testing.T) {
			replicas, limited, _, err := Desired(policy, counterFleet(tc), servers, nil, noon, neverCalled(t))

			assert.EqualError(t, err, "bufferSize should be between 1% and 99%, and is 100%")
			assert.Zero(t, replicas)
			assert.False(t, limited)
		})
	}
}

func TestCapacityPolicyThatCannotBeAppliedIsAnError(t *testing.T) {
	withRooms := counterFleet(capacityCase{perReplica: usage{0, 7}, replicas: 10, total: usage{68, 70}})
	withPlayers := listFleet(capacityCase{perReplica: usage{0, 7}, replicas: 10, total: usage{68, 70}})
	tests := map[string]struct {
		fleet  *agonesv1.Fleet
		policy autoscalingv1.FleetAutoscalerPolicy
		want   string
	}{
		"the Counter is not in the Fleet's template": {
			fleet:  fleetOf(func(f *agonesv1.Fleet) { f.Status.Counters = withRooms.Status.Counters }),
			policy: counterPolicy(amount(10), 10, 100),
			want:   "cannot apply CounterPolicy as Counter key rooms does not exist in the Fleet Spec",
		},
		"the Counter is not in the Fleet's status": {
			fleet:  fleetOf(func(f *agonesv1.Fleet) { f.Spec.Template = withRooms.Spec.Template }),
			policy: counterPolicy(amount(10), 10, 100),
			want:   "cannot apply CounterPolicy as Counter key rooms does not exist in the Fleet Status",
		},
		"the List is not in the Fleet's template": {
			fleet:  fleetOf(func(f *agonesv1.Fleet) { f.Status.Lists = withPlayers.Status.Lists }),
			policy: listPolicy(amount(10), 10, 100),
			want:   "cannot apply ListPolicy as List key players does not exist in the Fleet Spec",
		},
		"the List is not in the Fleet's status": {
			fleet:  fleetOf(func(f *agonesv1.Fleet) { f.Spec.Template = withPlayers.Spec.Template }),
			policy: listPolicy(amount(10), 10, 100),
			want:   "cannot apply ListPolicy as List key players does not exist in the Fleet Status",
		},
		"no Counter parameters at all": {
			fleet:  withRooms,
			policy: autoscalingv1.FleetAutoscalerPolicy{Type: counterType},
			want:   "counterPolicy parameter must not be nil",
		},
		"no List parameters at all": {
			fleet:  withPlayers,
			policy: autoscalingv1.FleetAutoscalerPolicy{Type: listType},
			want:   "listPolicy parameter must not be nil",
		},
		"a buffer that is not a percentage": {
			fleet:  withRooms,
			policy: counterPolicy(share("many"), 10, 100),
			want:   "invalid value for IntOrString: invalid type: string is not a percentage",
		},
		"a new replica would bring no free capacity": {
			fleet:  counterFleet(capacityCase{perReplica: usage{7, 7}, replicas: 10, total: usage{68, 70}}),
			policy: counterPolicy(amount(10), 10, 100),
			want:   "cannot scale up as adding additional replicas does not increase available Capacity",
		},
		"a new replica would bring no capacity toward the minimum": {
			fleet:  counterFleet(capacityCase{perReplica: usage{0, 0}, replicas: 1, total: usage{0, 0}}),
			policy: counterPolicy(amount(10), 10, 100),
			want:   "cannot scale up as Capacity is equal to 0",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			_, _, _, err := Desired(tc.policy, tc.fleet, nil, nil, noon, neverCalled(t))

			assert.EqualError(t, err, tc.want)
		})
	}
}
