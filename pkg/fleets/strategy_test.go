package fleets

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
)

func TestResizeWhenTheOldSetsHaveNothingMoreToGiveUp(t *testing.T) {
	tests := map[string]struct {
		old        []shape
		wantActive int32
	}{
		"a Fleet with only its active set gives it its whole size": {
			wantActive: 10,
		},
		"sessions in an old set take room from the active set": {
			old:        []shape{{replicas: 1, allocated: 1}},
			wantActive: 9,
		},
		"so do Reserved GameServers": {
			old:        []shape{{replicas: 3, allocated: 1, reserved: 2}, {replicas: 1, reserved: 1}},
			wantActive: 6,
		},
		"more of them than the Fleet's size leave no room at all": {
			old:        []shape{{replicas: 12, allocated: 12}},
			wantActive: 0,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			fleet := newFleet(t)
			fleet.Spec.Replicas = 10
			rest := olds(fleet, tc.old)

			replicas, shrunk, err := resize(fleet, fleet.GameServerSet(), rest)

			require.NoError(t, err)
			assert.Equal(t, tc.wantActive, replicas)
			assert.Empty(t, shrunk)
		})
	}
}

func TestRecreateEmptiesTheOldSetsAtOnce(t *testing.T) {
	tests := map[string]struct {
		old        []shape
		wantActive int32
		wantOld    sizes
	}{
		// Upstream: TestControllerRecreateDeployment.
		"the active set gets the Fleet's size less the sessions in old sets": {
			old:        []shape{full(10), {replicas: 1, allocated: 1}},
			wantActive: 9,
			wantOld:    sizes{"old-0": 0},
		},
		// Upstream counts Reserved ones only when every old set is at zero.
		"Reserved GameServers in old sets take no room yet": {
			old:        []shape{{spec: 10, replicas: 10, ready: 7, reserved: 3}},
			wantActive: 10,
			wantOld:    sizes{"old-0": 0},
		},
		"more sessions than the Fleet's size leave no room at all": {
			old:        []shape{{spec: 12, replicas: 12, allocated: 12}},
			wantActive: 0,
			wantOld:    sizes{"old-0": 0},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			fleet := newFleet(t)
			fleet.Spec.Strategy.Type = appsv1.RecreateDeploymentStrategyType
			fleet.Spec.Replicas = 10

			replicas, shrunk, err := resize(fleet, fleet.GameServerSet(), olds(fleet, tc.old))

			require.NoError(t, err)
			assert.Equal(t, tc.wantActive, replicas)
			assert.Equal(t, tc.wantOld, shrunk)
		})
	}
}

// A Fleet of 10 with two sessions running gets a new template. The sessions
// are left alone, and the Fleet is back at its size when they have ended.
func TestRecreateFromStartToFinish(t *testing.T) {
	fleet := newFleet(t)
	fleet.Spec.Strategy.Type = appsv1.RecreateDeploymentStrategyType
	fleet.Spec.Replicas = 10
	sets := list(old(fleet, "old", shape{spec: 10, replicas: 10, ready: 8, allocated: 2}))

	sets = pass(t, fleet, sets)
	assert.Equal(t, "old:0 new:8", specs(sets))

	settle(sets)
	sets = pass(t, fleet, sets)
	assert.Equal(t, "old:0 new:8", specs(sets), "the sessions keep the old set and their room in the Fleet")

	sets[0].Status.AllocatedReplicas = 0 // the sessions end
	settle(sets)
	sets = pass(t, fleet, sets)
	assert.Equal(t, "new:10", specs(sets))
}

// olds returns one old set for each shape, named old-0, old-1 and so on.
func olds(fleet *agonesv1.Fleet, shapes []shape) []*agonesv1.GameServerSet {
	var sets []*agonesv1.GameServerSet
	for i, s := range shapes {
		sets = append(sets, old(fleet, fmt.Sprintf("old-%d", i), s))
	}
	return sets
}

// pass is one round of what the caller of Plan does: it carries out the
// Changes and stores the Fleet's status. A created set is called "new".
func pass(t *testing.T, fleet *agonesv1.Fleet, sets []*agonesv1.GameServerSet) []*agonesv1.GameServerSet {
	t.Helper()
	changes, err := Plan(fleet, sets)
	require.NoError(t, err)

	var after []*agonesv1.GameServerSet
	for _, set := range sets {
		if slices.Contains(changes.Delete, set.Name) {
			continue
		}
		if changes.Update != nil && changes.Update.Name == set.Name {
			set = changes.Update
		}
		if replicas, resized := changes.Scale[set.Name]; resized {
			set.Spec.Replicas = replicas
		}
		after = append(after, set)
	}
	if changes.Create != nil {
		changes.Create.Name = "new"
		after = append(after, changes.Create)
	}
	fleet.Status = Status(fleet, after)
	return after
}

// settle is what happens between passes when all goes well: every set
// reaches its size, and its GameServers are Ready unless they are Allocated.
func settle(sets []*agonesv1.GameServerSet) {
	for _, set := range sets {
		allocated := set.Status.AllocatedReplicas
		others := max(set.Spec.Replicas-allocated, 0)
		set.Status = agonesv1.GameServerSetStatus{
			Replicas:          allocated + others,
			ReadyReplicas:     others,
			AllocatedReplicas: allocated,
		}
	}
}

// specs shows the spec.replicas of every set, like "old:75 new:25".
func specs(sets []*agonesv1.GameServerSet) string {
	var out []string
	for _, set := range sets {
		out = append(out, fmt.Sprintf("%s:%d", set.Name, set.Spec.Replicas))
	}
	return strings.Join(out, " ")
}
