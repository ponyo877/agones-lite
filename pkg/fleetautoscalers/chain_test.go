package fleetautoscalers

import (
	"errors"
	"testing"

	autoscalingv1 "agones.dev/agones/pkg/apis/autoscaling/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admregv1 "k8s.io/api/admissionregistration/v1"
)

// The entries of upstream's TestApplyChainPolicy. With the Fleet of
// fleetOf, each of them asks for its minimum.
var (
	// since2024 is active from August 2024 on.
	since2024 = entry("schedule-1", schedulePolicy(
		autoscalingv1.SchedulePolicy{
			Between:      between("2024-08-01T10:07:36-06:00", "").Between,
			ActivePeriod: autoscalingv1.ActivePeriod{Timezone: "America/Chicago", StartCron: "* * * * *"},
		},
		bufferPolicy(amount(1), 10, 10),
	))
	// until2021 was active until January 2021.
	until2021 = entry("schedule-2", schedulePolicy(
		autoscalingv1.SchedulePolicy{
			Between:      between("", "2021-01-02T04:53:00-05:00").Between,
			ActivePeriod: autoscalingv1.ActivePeriod{Timezone: "America/New_York", StartCron: "0 1 3 * *"},
		},
		bufferPolicy(amount(1), 3, 10),
	))
	// toService is a webhook that cannot be called here.
	toService = entry("webhook policy", autoscalingv1.FleetAutoscalerPolicy{
		Type:    webhookType,
		Webhook: &autoscalingv1.URLConfiguration{Service: &admregv1.ServiceReference{Namespace: "default", Name: "service1"}},
	})
	fallback = entry("default", bufferPolicy(amount(1), 6, 10))
)

// Upstream: TestApplyChainPolicy.
func TestChainTakesTheFirstEntryThatApplies(t *testing.T) {
	tests := map[string]struct {
		chain   []autoscalingv1.ChainEntry
		now     string
		want    int32
		applied autoscalingv1.FleetAutoscalerPolicyType
	}{
		"a single entry": {
			chain: []autoscalingv1.ChainEntry{fallback},
			now:   "2021-11-05T05:30:00Z",
			want:  6, applied: "Chain:default:Buffer",
		},
		"a webhook that fails passes the turn": {
			chain: []autoscalingv1.ChainEntry{toService, fallback},
			now:   "2021-11-05T05:30:00Z",
			want:  6, applied: "Chain:default:Buffer",
		},
		"schedules that are not active pass the turn": {
			chain: []autoscalingv1.ChainEntry{since2024, until2021, fallback},
			now:   "2021-11-05T05:30:00Z",
			want:  6, applied: "Chain:default:Buffer",
		},
		"an active schedule is as far as the chain goes": {
			chain: []autoscalingv1.ChainEntry{since2024, until2021, fallback},
			now:   "2024-08-01T10:07:36-06:00",
			want:  10, applied: "Chain:schedule-1:Schedule",
		},
		"an entry that applies hides the ones after it": {
			chain: []autoscalingv1.ChainEntry{fallback, since2024},
			now:   "2024-08-01T10:07:36-06:00",
			want:  6, applied: "Chain:default:Buffer",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			replicas, limited, applied, err := Desired(chainPolicy(tc.chain...), fleetOf(), nil, nil, at(tc.now), neverCalled(t))

			require.NoError(t, err)
			assert.Equal(t, tc.want, replicas)
			assert.True(t, limited, "2 Allocated and 1 free is less than any of the minimums")
			assert.Equal(t, tc.applied, applied)
		})
	}
}

func TestChainWithoutAnEntryThatAppliesIsAnError(t *testing.T) {
	down := errors.New("the webhook is down")
	tests := map[string]struct {
		chain []autoscalingv1.ChainEntry
		want  error
	}{
		"every schedule is inactive": {
			chain: []autoscalingv1.ChainEntry{since2024, since2024},
			want:  ErrInactiveSchedule,
		},
		"the error is that of the last entry": {
			chain: []autoscalingv1.ChainEntry{since2024, entry("ask", webhookPolicy(scaleURL))},
			want:  down,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			policy := chainPolicy(tc.chain...)

			replicas, limited, applied, err := Desired(policy, fleetOf(), nil, nil, at("2021-01-01T00:30:00Z"), answering(nil, down))

			assert.ErrorIs(t, err, tc.want)
			assert.Zero(t, replicas)
			assert.False(t, limited)
			assert.Empty(t, applied)
		})
	}
}

func TestChainInsideAnotherPolicyKeepsItsEntryToItself(t *testing.T) {
	inner := chainPolicy(fallback)
	tests := map[string]struct {
		policy  autoscalingv1.FleetAutoscalerPolicy
		applied autoscalingv1.FleetAutoscalerPolicyType
	}{
		"in a schedule": {
			policy:  schedulePolicy(between("", ""), inner),
			applied: scheduleType,
		},
		"in another chain": {
			policy:  chainPolicy(entry("outer", inner)),
			applied: "Chain:outer:Chain",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			replicas, _, applied, err := Desired(tc.policy, fleetOf(), nil, nil, noon, neverCalled(t))

			require.NoError(t, err)
			assert.Equal(t, int32(6), replicas)
			assert.Equal(t, tc.applied, applied)
		})
	}
}
