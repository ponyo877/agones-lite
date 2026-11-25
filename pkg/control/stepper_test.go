package control

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ponyo877/scrim/pkg/store"
)

// recorder is a Loop that writes down what it was asked to look at.
type recorder struct {
	everything []store.Key
	looked     []store.Key
	fail       error
}

func (r *recorder) loop() Loop {
	return Loop{
		Sync: func(_ context.Context, key store.Key) error {
			r.looked = append(r.looked, key)
			return r.fail
		},
		All: func() []store.Key { return r.everything },
	}
}

// subscription is a Watch on a channel the test fills.
func subscription(events chan store.Event) Watch {
	return Watch{Subscribe: func() (<-chan store.Event, func()) { return events, func() {} }, Keys: Self}
}

func TestAStepperDoesNothingUntilItIsTold(t *testing.T) {
	r := &recorder{everything: []store.Key{named("b"), named("a")}}
	var stepper Stepper
	stepper.Add("servers", r.loop())

	pending := stepper.Pending()

	assert.Equal(t, []Step{{"servers", named("a")}, {"servers", named("b")}}, pending,
		"everything waits at the start, in an order that does not depend on All")
	assert.Empty(t, r.looked)
}

func TestAStepperRunsTheStepItIsGivenAndNoOther(t *testing.T) {
	r := &recorder{everything: []store.Key{named("a"), named("b")}}
	var stepper Stepper
	stepper.Add("servers", r.loop())

	require.NoError(t, stepper.Run(context.Background(), Step{"servers", named("b")}))

	assert.Equal(t, []store.Key{named("b")}, r.looked)
	assert.Equal(t, []Step{{"servers", named("a")}}, stepper.Pending())
}

func TestAStepperQueuesWhatChangedOnce(t *testing.T) {
	events := make(chan store.Event, 3)
	r := &recorder{}
	var stepper Stepper
	stepper.Add("servers", r.loop(), subscription(events))
	events <- store.Event{Type: store.Modified, Key: named("changed")}
	events <- store.Event{Type: store.Modified, Key: named("changed")}

	assert.Equal(t, []Step{{"servers", named("changed")}}, stepper.Pending())
}

func TestAStepThatFailsWaitsAgain(t *testing.T) {
	r := &recorder{everything: []store.Key{named("a")}, fail: errors.New("try again later")}
	var stepper Stepper
	stepper.Add("servers", r.loop())

	err := stepper.Run(context.Background(), Step{"servers", named("a")})

	require.Error(t, err)
	assert.Equal(t, []Step{{"servers", named("a")}}, stepper.Pending())
}

func TestAStepperWhoseSubscriptionEndedLooksAtEverything(t *testing.T) {
	events := make(chan store.Event)
	r := &recorder{}
	var stepper Stepper
	stepper.Add("servers", r.loop(), Watch{
		Subscribe: func() (<-chan store.Event, func()) { return events, func() {} },
		Keys:      Self,
	})
	require.Empty(t, stepper.Pending())

	// What the store does to a watcher that fell too far behind.
	r.everything = []store.Key{named("missed")}
	lost := events
	events = make(chan store.Event)
	close(lost)

	assert.Equal(t, []Step{{"servers", named("missed")}}, stepper.Pending())
}

func TestResyncQueuesEverythingAgain(t *testing.T) {
	r := &recorder{everything: []store.Key{named("a")}}
	var stepper Stepper
	stepper.Add("servers", r.loop())
	require.NoError(t, stepper.Run(context.Background(), Step{"servers", named("a")}))
	require.Empty(t, stepper.Pending())

	stepper.Resync()

	assert.Equal(t, []Step{{"servers", named("a")}}, stepper.Pending())
}
