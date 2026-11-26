package gameservers

import (
	"testing"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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

// The tests below tie the Quint models in formal/quint to this package. The
// model checker tries every order of events on models of Next, NextStage
// and decide; what it finds holds for the Go functions as far as these are
// the same functions. The models' tables are written out whole, so passing
// means they are.

// quintEvent is the Event a model calls by name. The models have a name for
// "no event", and cannot call the allocation API's event Allocated, which
// is the name of a state there.
func quintEvent(name string) Event {
	switch name {
	case "AllocatedByRequest":
		return Allocated
	case "Nothing":
		return nothing
	}
	return Event(name)
}

func TestNextAgreesWithTheQuintModel(t *testing.T) {
	type vector struct {
		State, Event, Next string
		OK                 bool `json:"ok"`
	}
	all := vectors.Read[vector](t, "quint/transitions.jsonl")
	events, states := map[Event]bool{}, map[string]bool{}
	for _, v := range all {
		events[quintEvent(v.Event)], states[v.State] = true, true

		next, ok := Next(agonesv1.GameServerState(v.State), quintEvent(v.Event))

		assert.Equal(t, v.OK, ok, "whether %s applies on %s", v.Event, v.State)
		assert.Equal(t, agonesv1.GameServerState(v.Next), next, "%s on %s", v.Event, v.State)
	}

	// Nothing either Go table knows is missing from the model.
	for event := range rules {
		assert.True(t, events[event], "the model has no event %q", event)
	}
	for event := range stageRules {
		assert.True(t, events[event], "the model has no event %q", event)
	}
	assert.Len(t, states, 11)
	// And everyState, which In goes through, has every state the model has.
	for state := range states {
		assert.Contains(t, everyState, agonesv1.GameServerState(state))
	}
	assert.Len(t, all, len(states)*len(events))
}

func TestNextStageAgreesWithTheQuintModel(t *testing.T) {
	type vector struct {
		Stage, Event, Next string
		OK                 bool `json:"ok"`
	}
	all := vectors.Read[vector](t, "quint/stages.jsonl")
	events := map[Event]bool{}
	for _, v := range all {
		events[quintEvent(v.Event)] = true

		next, ok := NextStage(Stage(v.Stage), quintEvent(v.Event))

		assert.Equal(t, v.OK, ok, "whether %s applies to a record that is %s", v.Event, v.Stage)
		assert.Equal(t, Stage(v.Next), next, "%s on a record that is %s", v.Event, v.Stage)
	}
	for event := range stageRules {
		assert.True(t, events[event], "the model has no event %q", event)
	}
	assert.Len(t, all, 2*len(events), "every event, on a record that is Live and on one that is Leaving")
}

func TestDecideAgreesWithTheQuintModel(t *testing.T) {
	type vector struct {
		State, Stage, Event string
		Placed, Reachable   bool
	}
	all := vectors.Read[vector](t, "quint/decisions.jsonl")
	for _, v := range all {
		gs := &agonesv1.GameServer{}
		gs.Status.State = agonesv1.GameServerState(v.State)
		if v.Placed {
			gs.Status.NodeName = "node-1"
		}
		if Stage(v.Stage) == Leaving {
			gs.DeletionTimestamp = &metav1.Time{Time: noon}
		}

		event := decide(gs, v.Reachable)

		assert.Equal(t, quintEvent(v.Event), event, "%+v", v)
	}
	assert.Len(t, all, 11*2*2*2, "every state, Live and Leaving, placed or not, reachable or not")
}
