package fleets

import (
	"fmt"
	"testing"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	"github.com/stretchr/testify/require"

	"github.com/ponyo877/scrim/formal/vectors"
)

// setVector is a GameServerSet as formal/Formal/Rollout.lean has it.
type setVector struct {
	Spec      int32 `json:"spec"`
	Replicas  int32 `json:"replicas"`
	Ready     int32 `json:"ready"`
	Reserved  int32 `json:"reserved"`
	Allocated int32 `json:"allocated"`
	Shutdown  int32 `json:"shutdown"`
}

func (v setVector) shape() shape {
	return shape{spec: v.Spec, replicas: v.Replicas, ready: v.Ready, reserved: v.Reserved, allocated: v.Allocated, shutdown: v.Shutdown}
}

// rolloutVector is one pass over a Fleet, and what the model answers.
type rolloutVector struct {
	Replicas   int32       `json:"replicas"`
	Surge      int32       `json:"surge"`
	Percent    int32       `json:"percent"` // maxUnavailable
	FleetReady int32       `json:"fleetReady"`
	Active     setVector   `json:"active"`
	Rest       []setVector `json:"rest"`
	Grow       int32       `json:"grow"`
	Shrink     []int32     `json:"shrink"`
}

// fleetRolling returns a Fleet that rolls by the given surge and percentage.
func fleetRolling(t *testing.T, replicas, surge, percent, ready int32) *agonesv1.Fleet {
	fleet := newFleet(t)
	fleet.Spec.Replicas, fleet.Status.ReadyReplicas = replicas, ready
	rollingBy(fleet, fmt.Sprint(surge), fmt.Sprint(percent, "%"))
	return fleet
}

// The theorems in formal/Formal/Rollout.lean are about a model. This test
// ties them to grow and shrink: on every input the model was run on, the
// two must answer alike.
func TestOnePassOfARolloutAgreesWithTheLeanModel(t *testing.T) {
	all := vectors.Read[rolloutVector](t, "rollout.jsonl")
	require.NotEmpty(t, all)
	for i, v := range all {
		fleet := fleetRolling(t, v.Replicas, v.Surge, v.Percent, v.FleetReady)
		active := current(fleet, "active", v.Active.shape())
		var rest []*agonesv1.GameServerSet
		for j, set := range v.Rest {
			rest = append(rest, current(fleet, fmt.Sprint("old-", j), set.shape()))
		}

		grown, err := grow(fleet, active, rest)
		require.NoError(t, err)
		shrunk, err := shrink(fleet, active, rest)
		require.NoError(t, err)

		require.Equal(t, v.Grow, grown, "vector %d, grow: %+v", i, v)
		for j, set := range rest {
			require.Equal(t, v.Shrink[j], shrunk.of(set), "vector %d, shrink of the set at %d: %+v", i, j, v)
		}
	}
}

// stuckVector is the rollout the model plays in
// a_version_that_never_gets_ready_takes_most_of_the_fleet.
type stuckVector struct {
	Replicas int32   `json:"replicas"`
	Surge    int32   `json:"surge"`
	Percent  int32   `json:"percent"`
	Old      []int32 `json:"old"` // the old set's size after each pass
	New      []int32 `json:"new"`
}

// A Fleet is given a version that never becomes Ready. Between passes every
// status catches up, and what is left of the old set is all Ready. The
// floor under the Ready GameServers follows them down, as the model says it
// does.
func TestARolloutOfAVersionThatNeverGetsReadyGoesAsTheLeanModelSays(t *testing.T) {
	for _, v := range vectors.Read[stuckVector](t, "stuck.jsonl") {
		older, newer := v.Replicas, int32(0)
		for pass := range v.Old {
			fleet := fleetRolling(t, v.Replicas, v.Surge, v.Percent, older)
			active := current(fleet, "new", shape{spec: newer, replicas: newer})
			rest := current(fleet, "old", full(older))

			grown, shrunk, err := rollingUpdate(fleet, active, []*agonesv1.GameServerSet{rest})
			require.NoError(t, err)
			older, newer = shrunk.of(rest), grown

			require.Equal(t, v.Old[pass], older, "the old set after pass %d", pass+1)
			require.Equal(t, v.New[pass], newer, "the new set after pass %d", pass+1)
		}
	}
}
