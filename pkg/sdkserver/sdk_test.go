package sdkserver

import (
	"context"
	"math"
	"slices"
	"testing"
	"time"

	"agones.dev/agones/pkg/sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	sidecarv1 "github.com/ponyo877/scrim/proto/sidecar/v1"
)

func TestReserveWithoutAUsableTimeLastsUntilToldOtherwise(t *testing.T) {
	const longest = math.MaxInt64 / int64(time.Second) // what a time.Duration holds: 292 years
	tests := map[string]struct {
		seconds int64
		told    int64
	}{
		"zero means until told otherwise": {seconds: 0, told: 0},
		"negative counts as zero":         {seconds: -5, told: 0}, // as upstream has it
		"one second too many":             {seconds: longest + 1, told: longest},
		"317 years":                       {seconds: 10_000_000_000, told: longest},
		"as large as it gets":             {seconds: math.MaxInt64, told: longest},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			agent := &fakeAgent{}
			s := New(agent)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go s.deliver(ctx)

			_, err := s.Reserve(ctx, &sdk.Duration{Seconds: tc.seconds})
			require.NoError(t, err)

			returned := func() bool { return slices.Contains(agent.events(), sidecarv1.Event_EVENT_RESERVE_EXPIRED) }
			assert.Never(t, returned, 200*time.Millisecond, 10*time.Millisecond, "the time must not be up at once")
			require.Equal(t, []sidecarv1.Event{sidecarv1.Event_EVENT_RESERVE}, agent.events())
			assert.Equal(t, tc.told, agent.told[0].GetReserveSeconds())
		})
	}
}
