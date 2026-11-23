package fleets

import (
	"testing"

	"agones.dev/agones/pkg/apis"
	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The cases of upstream's TestControllerSyncFleet,
// TestControllerUpsertGameServerSet and TestControllerDeleteEmptyGameServerSets
// are among the tests of Plan below.

func TestPlanCreatesTheFirstSetAtTheFleetsSize(t *testing.T) {
	fleet := newFleet(t)

	changes, err := Plan(fleet, nil)

	require.NoError(t, err)
	want := fleet.GameServerSet()
	want.Spec.Replicas = 5
	assert.Equal(t, Changes{Create: want}, changes)
	assert.True(t, metav1.IsControlledBy(changes.Create, fleet))
	assert.Empty(t, changes.Create.Name, "the store makes a name from generateName")
}

func TestPlanLeavesASetThatMatchesTheFleetAlone(t *testing.T) {
	fleet := newFleet(t)

	changes, err := Plan(fleet, list(current(fleet, "active", full(5))))

	require.NoError(t, err)
	assert.Equal(t, Changes{}, changes)
}

func TestPlanResizesTheActiveSetWithTheFleet(t *testing.T) {
	tests := map[string]struct {
		active shape
	}{
		"a Fleet that grew":   {active: full(3)},
		"a Fleet that shrank": {active: full(15)},
		// No old set is in the way, so the size is not taken in steps.
		"a set that has not reached its last size yet": {active: shape{spec: 15, replicas: 9, ready: 2}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			fleet := newFleet(t)

			changes, err := Plan(fleet, list(current(fleet, "active", tc.active)))

			require.NoError(t, err)
			assert.Equal(t, Changes{Scale: map[string]int32{"active": 5}}, changes)
		})
	}
}

func TestPlanCopiesSchedulingAndPrioritiesToTheActiveSet(t *testing.T) {
	byRooms := []agonesv1.Priority{{Type: "Counter", Key: "rooms", Order: "Ascending"}}
	tests := map[string]struct {
		// What the Fleet says after the active set was made from it.
		scheduling apis.SchedulingStrategy
		priorities []agonesv1.Priority
		replicas   int32
		updated    bool
	}{
		"another scheduling strategy": {
			scheduling: apis.Distributed, replicas: 5, updated: true,
		},
		"other priorities": {
			scheduling: apis.Packed, priorities: byRooms, replicas: 5, updated: true,
		},
		"both, and another size at the same time": {
			scheduling: apis.Distributed, priorities: byRooms, replicas: 8, updated: true,
		},
		// Upstream compares with cmp.Equal, which tells an empty list from
		// none, and so updates the set on every pass.
		"an empty list of priorities where the set has none": {
			scheduling: apis.Packed, priorities: []agonesv1.Priority{}, replicas: 5,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			fleet := newFleet(t)
			active := current(fleet, "active", full(5))
			fleet.Spec.Scheduling, fleet.Spec.Priorities, fleet.Spec.Replicas = tc.scheduling, tc.priorities, tc.replicas

			changes, err := Plan(fleet, list(active))

			require.NoError(t, err)
			if !tc.updated {
				assert.Equal(t, Changes{}, changes)
				return
			}
			want := current(fleet, "active", full(5)) // as the Fleet would make it now
			want.Spec.Replicas = tc.replicas
			assert.Equal(t, Changes{Update: want}, changes)
			assert.Equal(t, apis.Packed, active.Spec.Scheduling, "the set Plan was given stays as it was")
		})
	}
}

func TestPlanDeletesOldSetsOnceTheyAreEmpty(t *testing.T) {
	tests := map[string]struct {
		old     shape
		deleted bool
	}{
		"an old set without GameServers":                       {old: shape{}, deleted: true},
		"an old set that still has GameServers":                {old: full(10)},
		"an old set with nothing left but a session":           {old: shape{replicas: 1, allocated: 1}},
		"an old set whose last GameServers are shutting down":  {old: shape{shutdown: 2}},
		"an old set that asks for GameServers and has not one": {old: shape{spec: 3}, deleted: true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			fleet := newFleet(t)
			sets := list(current(fleet, "active", full(5)), old(fleet, "old", tc.old))
			seenReady(fleet, sets...)

			changes, err := Plan(fleet, sets)

			require.NoError(t, err)
			if !tc.deleted {
				assert.Empty(t, changes.Delete)
				return
			}
			assert.Equal(t, []string{"old"}, changes.Delete)
			assert.NotContains(t, changes.Scale, "old", "there is no point in resizing a set that goes")
		})
	}
}

// Upstream leaves the GameServerSets of such a Fleet to the garbage
// collector of Kubernetes.
func TestPlanDoesNothingForAFleetThatIsBeingDeleted(t *testing.T) {
	fleet := newFleet(t)
	sets := list(old(fleet, "old", full(5)))
	seenReady(fleet, sets...)
	fleet.DeletionTimestamp = &metav1.Time{Time: noon}

	changes, err := Plan(fleet, sets)

	require.NoError(t, err)
	assert.Equal(t, Changes{}, changes)
}

func TestPlanGivesUpOnAFleetThatAdmissionWouldRefuse(t *testing.T) {
	tests := map[string]struct {
		spoil   func(*agonesv1.Fleet)
		wantErr string
	}{
		"an unknown strategy": {
			spoil:   func(f *agonesv1.Fleet) { f.Spec.Strategy.Type = "Sideways" },
			wantErr: "unexpected deployment strategy type: Sideways",
		},
		"a rolling update without maxSurge": {
			spoil:   func(f *agonesv1.Fleet) { f.Spec.Strategy.RollingUpdate.MaxSurge = nil },
			wantErr: "parsing maxSurge: nil value for IntOrString",
		},
		"a rolling update without maxUnavailable": {
			spoil:   func(f *agonesv1.Fleet) { f.Spec.Strategy.RollingUpdate.MaxUnavailable = nil },
			wantErr: "parsing maxUnavailable: nil value for IntOrString",
		},
		"a maxUnavailable that is neither a number nor a percentage": {
			spoil:   func(f *agonesv1.Fleet) { rollingBy(f, "25%", "some") },
			wantErr: "parsing maxUnavailable: invalid value for IntOrString",
		},
		// Upstream panics here.
		"a rolling update without any settings": {
			spoil:   func(f *agonesv1.Fleet) { f.Spec.Strategy.RollingUpdate = nil },
			wantErr: "parsing maxSurge: nil value for IntOrString",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			fleet := newFleet(t)
			sets := list(old(fleet, "old", full(5)))
			seenReady(fleet, sets...)
			tc.spoil(fleet)

			changes, err := Plan(fleet, sets)

			assert.ErrorContains(t, err, tc.wantErr)
			assert.Equal(t, Changes{}, changes)
		})
	}
}

// The strategy is only looked at when there is an old set to replace.
func TestPlanDoesNotMindAnUnknownStrategyWithoutOldSets(t *testing.T) {
	fleet := newFleet(t)
	fleet.Spec.Strategy.Type = "Sideways"

	changes, err := Plan(fleet, list(current(fleet, "active", full(3))))

	require.NoError(t, err)
	assert.Equal(t, Changes{Scale: map[string]int32{"active": 5}}, changes)
}

func TestActive(t *testing.T) {
	fleet := newFleet(t)
	first := born(current(fleet, "first", shape{}), 1)
	second := born(current(fleet, "second", shape{}), 2)
	third := born(current(fleet, "third", shape{}), 3)
	stale := born(old(fleet, "stale", shape{}), 0)

	tests := map[string]struct {
		sets   []*agonesv1.GameServerSet
		active *agonesv1.GameServerSet
		rest   []*agonesv1.GameServerSet
	}{
		"a Fleet without sets has neither": {},
		"the set made from the current template is active": {
			sets: list(first, stale), active: first, rest: list(stale),
		},
		"no set is active once the template has changed": {
			sets: list(stale), rest: list(stale),
		},
		// Upstream meets such duplicates when its cache lags behind a creation.
		"of several sets from the current template the oldest is active": {
			sets: list(first, second, third), active: first, rest: list(second, third),
		},
		"the oldest is found wherever it is in the list": {
			sets: list(third, first, second), active: first, rest: list(third, second),
		},
		"the duplicates join the old sets": {
			sets: list(third, stale, first, second), active: first, rest: list(stale, third, second),
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			active, rest := Active(fleet, tc.sets)

			assert.Equal(t, tc.active, active)
			assert.Equal(t, tc.rest, rest)
		})
	}
}

// Upstream: TestResourcesRequestsAndLimits.
func TestActiveComparesTemplatesByMeaning(t *testing.T) {
	withCPU := func(set *agonesv1.GameServerSet, cpu string) *agonesv1.GameServerSet {
		limits := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu)}
		set.Spec.Template.Spec.Template.Spec.Containers = []corev1.Container{{
			Name: "game", Resources: corev1.ResourceRequirements{Limits: limits},
		}}
		return set
	}
	fleet := newFleet(t)
	fleet.Spec.Template = withCPU(fleet.GameServerSet(), "1000m").Spec.Template
	oneCPU := withCPU(current(fleet, "one-cpu", shape{}), "1")
	more := withCPU(current(fleet, "more", shape{}), "1.1")

	active, rest := Active(fleet, list(more, oneCPU))

	assert.Equal(t, oneCPU, active, "1 CPU is 1000m")
	assert.Equal(t, list(more), rest)
}
