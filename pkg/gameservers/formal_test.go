package gameservers

import (
	"testing"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ponyo877/scrim/formal/vectors"
)

// counterVector is one answer of the model in formal/Formal/Counts.lean.
type counterVector struct {
	Count         int64  `json:"count"`
	Capacity      int64  `json:"capacity"`
	SetCapacity   *int64 `json:"setCapacity"`
	SetCount      *int64 `json:"setCount"`
	Diff          int64  `json:"diff"`
	AfterCount    int64  `json:"afterCount"`
	AfterCapacity int64  `json:"afterCapacity"`
}

// The theorems in formal/Formal/Counts.lean are about a model. This test
// ties them to counterAfter: on every input the model was run on, the two
// must answer alike.
func TestCounterAfterAgreesWithTheLeanModel(t *testing.T) {
	all := vectors.Read[counterVector](t, "counters.jsonl")
	require.NotEmpty(t, all)
	for i, v := range all {
		before := agonesv1.CounterStatus{Count: v.Count, Capacity: v.Capacity}
		update := CounterUpdate{Capacity: v.SetCapacity, Count: v.SetCount, CountDiff: v.Diff}

		after := counterAfter(before, update)

		want := agonesv1.CounterStatus{Count: v.AfterCount, Capacity: v.AfterCapacity}
		require.Equal(t, want, after, "vector %d: %+v", i, v)
	}
}

// transitionVector is one cell of the table in formal/Formal/StateMachine.lean.
type transitionVector struct {
	State string  `json:"state"`
	Event string  `json:"event"`
	Next  *string `json:"next"` // nil when the event does not apply
}

// The model's table has every state with every event, so passing this test
// means the model and Next are the same function.
func TestNextAgreesWithTheLeanModel(t *testing.T) {
	all := vectors.Read[transitionVector](t, "transitions.jsonl")
	events, states := map[Event]bool{}, map[string]bool{}
	for _, v := range all {
		events[Event(v.Event)], states[v.State] = true, true

		next, ok := Next(agonesv1.GameServerState(v.State), Event(v.Event))

		if v.Next == nil {
			assert.False(t, ok, "%s on %s must not apply", v.Event, v.State)
			continue
		}
		assert.True(t, ok, "%s on %s must apply", v.Event, v.State)
		assert.Equal(t, agonesv1.GameServerState(*v.Next), next, "%s on %s", v.Event, v.State)
	}

	// Nothing the Go table knows is missing from the model.
	for event := range rules {
		assert.True(t, events[event], "the model has no event %q", event)
	}
	assert.Len(t, states, 11)
	assert.Len(t, all, len(states)*len(events))
}
