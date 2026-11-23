package fleets

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRollingUpdateTakesOneStep(t *testing.T) {
	tests := map[string]struct {
		replicas       int32  // the Fleet's size
		maxSurge       string // 25% when empty
		maxUnavailable string // 25% when empty
		readyBefore    int32  // Ready at the last pass; as many as now when zero
		active         shape
		old            []shape
		wantActive     int32
		wantOld        []int32
	}{
		// Upstream: TestControllerRollingUpdateDeployment. Its old sets count
		// their Allocated GameServers among the Ready ones, and so do these.
		"the first step adds maxSurge and takes maxUnavailable away": {
			replicas: 100, maxUnavailable: "30%",
			active:     shape{},
			old:        []shape{full(100)},
			wantActive: 25, wantOld: []int32{70},
		},
		"the last step leaves room for the sessions in the old set": {
			replicas: 100, maxUnavailable: "30%",
			active:     full(75),
			old:        []shape{{spec: 10, replicas: 10, ready: 10, allocated: 5}},
			wantActive: 95, wantOld: []int32{0},
		},
		"a small Fleet rounds maxSurge up and maxUnavailable down": {
			replicas: 5, maxUnavailable: "30%",
			active:     shape{},
			old:        []shape{{spec: 5, replicas: 5, ready: 5, allocated: 2}},
			wantActive: 2, wantOld: []int32{4},
		},
		"an active set at full size makes room for sessions in the old set": {
			replicas: 75, maxUnavailable: "30%",
			active:     full(75),
			old:        []shape{{spec: 10, replicas: 10, ready: 10, allocated: 5}},
			wantActive: 70, wantOld: []int32{0},
		},
		"an old set full of sessions gives up only a few of its Ready GameServers": {
			replicas: 100, maxUnavailable: "30%",
			active:     shape{},
			old:        []shape{{spec: 100, replicas: 100, ready: 10, allocated: 90}},
			wantActive: 10, wantOld: []int32{97},
		},
		"an old set with nothing but sessions goes straight to zero": {
			replicas: 100, maxUnavailable: "30%",
			active:     full(50),
			old:        []shape{{spec: 50, replicas: 50, allocated: 50}},
			wantActive: 50, wantOld: []int32{0},
		},
		"a Fleet scaled to zero empties the old set": {
			replicas: 0, maxUnavailable: "30%",
			active:     shape{},
			old:        []shape{full(3)},
			wantActive: 0, wantOld: []int32{0},
		},

		// Upstream: TestRollingUpdateOnReady.
		"the old set waits while few GameServers of the active set are Ready": {
			replicas:   75,
			active:     shape{spec: 75, replicas: 75, ready: 10},
			old:        []shape{full(10)},
			wantActive: 75, wantOld: []int32{10},
		},
		"it still waits when the active set is two thirds Ready": {
			replicas:   75,
			active:     shape{spec: 75, replicas: 75, ready: 50},
			old:        []shape{{spec: 10, replicas: 10, ready: 8}},
			wantActive: 75, wantOld: []int32{10},
		},
		"it goes when nearly all of the active set is Ready": {
			replicas:   75,
			active:     shape{spec: 75, replicas: 75, ready: 70},
			old:        []shape{full(10)},
			wantActive: 75, wantOld: []int32{0},
		},
		"it goes with sessions too, and the active set makes room for those": {
			replicas:   75,
			active:     shape{spec: 75, replicas: 75, ready: 70},
			old:        []shape{{spec: 10, replicas: 10, ready: 5, allocated: 5}},
			wantActive: 70, wantOld: []int32{0},
		},

		// Upstream: TestControllerRollingUpdateDeploymentNegativeReplica and
		// TestControllerRollingUpdateDeploymentAllocatedExceedsFleetReplicas.
		"a session in an old set takes room from the active set": {
			replicas:   5,
			active:     full(4),
			old:        []shape{{replicas: 1, allocated: 1}},
			wantActive: 4, wantOld: []int32{0},
		},
		"a Fleet scaled to zero during a session asks for nothing, not for less": {
			replicas:   0,
			active:     full(4),
			old:        []shape{{replicas: 1, allocated: 1}},
			wantActive: 0, wantOld: []int32{0},
		},
		"more sessions than the Fleet's size leave the active set empty": {
			replicas:   5,
			active:     shape{},
			old:        []shape{{replicas: 8, allocated: 8}},
			wantActive: 0, wantOld: []int32{0},
		},

		// Not covered by upstream's tests.
		"the active set does not grow before its last step is done": {
			replicas:   10,
			active:     shape{spec: 6, replicas: 3, ready: 3},
			old:        []shape{full(7)},
			wantActive: 6, wantOld: []int32{7},
		},
		"the active set does not grow past the surge while the old set is still big": {
			replicas:   10,
			active:     full(3),
			old:        []shape{{spec: 7, replicas: 10, ready: 10}},
			wantActive: 3, wantOld: []int32{7},
		},
		"GameServers of the old set that are not up go first": {
			replicas:   10,
			active:     full(3),
			old:        []shape{{spec: 10, replicas: 10, ready: 6}},
			wantActive: 3, wantOld: []int32{8},
		},
		"old sets are scaled down one after the other": {
			replicas: 10, maxUnavailable: "30%",
			active:     full(5),
			old:        []shape{full(2), full(3), full(1)},
			wantActive: 7, wantOld: []int32{0, 2, 1},
		},
		"an old set that is still shutting GameServers down is passed over": {
			replicas: 10, maxUnavailable: "30%",
			active:     full(5),
			old:        []shape{{spec: 2, replicas: 2, ready: 2, shutdown: 1}, full(3)},
			wantActive: 8, wantOld: []int32{2, 0},
		},
		"an old set that is not down to its size yet holds up the ones after it": {
			replicas: 10, maxUnavailable: "30%",
			active:     full(5),
			old:        []shape{{spec: 2, replicas: 4, ready: 4}, full(3)},
			wantActive: 6, wantOld: []int32{2, 3},
		},
		// The Fleet may fall to maxUnavailable below what was Ready at the
		// last pass: 75 then, so 57. With 100 Ready now, 43 may go.
		"GameServers that became Ready since the last pass may go as well": {
			replicas: 100, readyBefore: 75,
			active:     shape{spec: 50, replicas: 50, ready: 25},
			old:        []shape{full(75)},
			wantActive: 50, wantOld: []int32{32},
		},
		"maxSurge and maxUnavailable can be numbers of GameServers": {
			replicas: 10, maxSurge: "4", maxUnavailable: "1",
			active:     shape{},
			old:        []shape{full(10)},
			wantActive: 4, wantOld: []int32{9},
		},
		"at least one GameServer may go, however small maxUnavailable is": {
			replicas: 3, maxUnavailable: "1%",
			active:     shape{},
			old:        []shape{full(3)},
			wantActive: 1, wantOld: []int32{2},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			fleet := newFleet(t)
			fleet.Spec.Replicas = tc.replicas
			rollingBy(fleet, orDefault(tc.maxSurge), orDefault(tc.maxUnavailable))
			active := current(fleet, "active", tc.active)
			rest := olds(fleet, tc.old)
			seenReady(fleet, append(rest, active)...)
			if tc.readyBefore != 0 {
				fleet.Status.ReadyReplicas = tc.readyBefore
			}

			replicas, shrunk, err := rollingUpdate(fleet, active, rest)

			require.NoError(t, err)
			assert.Equal(t, tc.wantActive, replicas, "size of the active set")
			for i, set := range rest {
				assert.Equal(t, tc.wantOld[i], shrunk.of(set), "size of %s", set.Name)
			}
		})
	}
}

func orDefault(percent string) string {
	if percent == "" {
		return "25%"
	}
	return percent
}

// A Fleet of 100 gets a new template, and its GameServers come up as fast
// as they are asked for. A quarter of the Fleet changes over in each pass.
func TestRollingUpdateFromStartToFinish(t *testing.T) {
	fleet := newFleet(t)
	fleet.Spec.Replicas = 100
	sets := list(old(fleet, "old", full(100)))
	seenReady(fleet, sets...)

	var history []string
	for range 6 {
		sets = pass(t, fleet, sets)
		history = append(history, specs(sets))
		settle(sets)
	}

	assert.Equal(t, []string{
		"old:75 new:25",
		"old:50 new:50",
		"old:25 new:75",
		"old:0 new:100",
		"new:100",
		"new:100",
	}, history)
}

// The same Fleet, but the GameServers of its new template never get Ready.
// The update stops, and three quarters of the Fleet go on serving.
func TestRollingUpdateStopsWhenNewGameServersDoNotGetReady(t *testing.T) {
	fleet := newFleet(t)
	fleet.Spec.Replicas = 100
	sets := list(old(fleet, "old", full(100)))
	seenReady(fleet, sets...)

	var history []string
	for range 4 {
		sets = pass(t, fleet, sets)
		history = append(history, specs(sets))
		settle(sets)
		sets[1].Status.ReadyReplicas = 0 // the new set
	}

	assert.Equal(t, []string{
		"old:75 new:25",
		"old:75 new:50",
		"old:75 new:50",
		"old:75 new:50",
	}, history)
}
