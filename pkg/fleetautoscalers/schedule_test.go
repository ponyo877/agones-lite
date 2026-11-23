package fleetautoscalers

import (
	"testing"

	autoscalingv1 "agones.dev/agones/pkg/apis/autoscaling/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scheduleCase is a schedule as a manifest gives it. Empty fields are left
// out of the manifest.
type scheduleCase struct {
	start, end         string // the window the schedule may be active in
	cron, zone, length string // the active periods inside the window
	now                string
	want               bool
}

func (c scheduleCase) schedule() autoscalingv1.SchedulePolicy {
	s := between(c.start, c.end)
	s.ActivePeriod = autoscalingv1.ActivePeriod{Timezone: c.zone, StartCron: c.cron, Duration: c.length}
	return s
}

// Upstream: TestApplySchedulePolicy. Its three cases named after daylight
// saving time and the new year are the ones with the same times here.
func TestScheduleIsActiveInsideItsWindowAndPeriods(t *testing.T) {
	const everyMinute = "* * * * *"
	tests := map[string]scheduleCase{
		"with nothing set, it is always active": {
			now: "2021-01-01T00:00:00Z", want: true,
		},
		"without a start, it is active until the end": {
			end: "2021-01-01T00:00:00Z", cron: everyMinute, zone: "UTC", length: "48h",
			now: "2020-12-26T08:30:00Z", want: true,
		},
		"without an end, it is active from the start on": {
			start: "2021-01-01T00:00:00Z", cron: everyMinute, zone: "UTC", length: "1h",
			now: "2021-01-02T00:00:00Z", want: true,
		},
		"before the start it is not active": {
			start: "2021-01-01T00:00:00Z",
			now:   "2020-12-31T23:59:59Z", want: false,
		},
		"after the end it is not active": {
			end: "2023-03-02T00:00:00Z", cron: "* 0 * 3 *", zone: "UTC",
			now: "2023-12-12T03:49:00Z", want: false,
		},
		"the start and the end themselves are inside the window": {
			start: "2021-01-01T00:00:00Z", end: "2021-01-01T00:00:00Z",
			now: "2021-01-01T00:00:00Z", want: true,
		},
		"without a cron expression, the duration means nothing": {
			start: "2021-01-01T00:00:00Z", end: "2021-01-02T00:00:00Z", zone: "UTC", length: "1h",
			now: "2021-01-01T12:00:00Z", want: true,
		},
		"without a duration, a period never ends": {
			cron: "0 0 1 1 *", zone: "UTC",
			now: "2021-06-15T12:00:00Z", want: true,
		},
		"a period starts at a cron time and lasts for the duration": {
			cron: "0 14 * * *", zone: "UTC", length: "1h",
			now: "2021-03-14T14:59:59Z", want: true,
		},
		"a period is over once the duration has passed": {
			cron: "0 14 * * *", zone: "UTC", length: "1h",
			now: "2021-03-14T15:00:00Z", want: false,
		},
		"a period begins 90 seconds early": {
			start: "2021-11-07T00:00:00Z", end: "2021-11-08T00:00:00Z", cron: "0 2 * * *", zone: "UTC", length: "1h",
			now: "2021-11-07T01:59:59Z", want: true,
		},
		"a period does not begin earlier than that": {
			cron: "0 2 * * *", zone: "UTC", length: "1h",
			now: "2021-11-07T01:58:30Z", want: false,
		},
		"periods that begin every minute of an hour cover the hour": {
			start: "2021-03-13T00:00:00Z", end: "2021-03-15T00:00:00Z", cron: "* 2 * * *", zone: "UTC", length: "1h",
			now: "2021-03-14T02:00:00Z", want: true,
		},
		"a period can begin with the new year": {
			end: "2021-01-02T00:00:00Z", cron: "* 0 * * *", zone: "UTC", length: "1h",
			now: "2021-01-01T00:00:00Z", want: true,
		},
		"cron times are read in the schedule's time zone": {
			cron: "0 9 * * *", zone: "Asia/Tokyo", length: "1h",
			now: "2021-06-01T00:30:00Z", want: true,
		},
		"so the same hour in UTC is outside the period": {
			cron: "0 9 * * *", zone: "Asia/Tokyo", length: "1h",
			now: "2021-06-01T09:30:00Z", want: false,
		},
		"without a time zone, cron times are in UTC": {
			cron: "0 9 * * *", length: "1h",
			now: "2021-06-01T09:30:00Z", want: true,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			s := tc.schedule()

			on, err := active(&s, at(tc.now))

			require.NoError(t, err)
			assert.Equal(t, tc.want, on)
		})
	}
}

func TestScheduleThatDoesNotParseIsAnError(t *testing.T) {
	tests := map[string]struct {
		period scheduleCase
		want   string
	}{
		"an unknown time zone": {
			period: scheduleCase{cron: "0 9 * * *", zone: "Mars/Olympus_Mons"},
			want:   "unknown time zone Mars/Olympus_Mons",
		},
		"a cron expression with too few fields": {
			period: scheduleCase{cron: "0 9 * *"},
			want:   "expected exactly 5 fields, found 4: [0 9 * *]",
		},
		"a duration without a unit": {
			period: scheduleCase{cron: "0 9 * * *", length: "90"},
			want:   `time: missing unit in duration "90"`,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			policy := schedulePolicy(tc.period.schedule(), bufferPolicy(amount(1), 3, 10))

			_, _, _, err := Desired(policy, fleetOf(), nil, nil, noon, neverCalled(t))

			assert.EqualError(t, err, tc.want)
		})
	}
}

func TestScheduleAppliesItsPolicyOnlyWhileActive(t *testing.T) {
	window := between("2026-11-23T00:00:00Z", "2026-11-24T00:00:00Z")
	policy := schedulePolicy(window, bufferPolicy(amount(1), 3, 10))

	t.Run("inside the window, the answer is that of the policy inside", func(t *testing.T) {
		replicas, limited, applied, err := Desired(policy, fleetOf(), nil, nil, noon, neverCalled(t))

		require.NoError(t, err)
		assert.Equal(t, int32(3), replicas)
		assert.False(t, limited)
		assert.Equal(t, scheduleType, applied)
	})

	t.Run("outside the window, there is no answer", func(t *testing.T) {
		replicas, limited, applied, err := Desired(policy, fleetOf(), nil, nil, noon.AddDate(0, 0, 1), neverCalled(t))

		assert.ErrorIs(t, err, ErrInactiveSchedule)
		assert.Zero(t, replicas)
		assert.False(t, limited)
		assert.Empty(t, applied)
	})

	t.Run("the policy inside may be a webhook", func(t *testing.T) {
		policy := schedulePolicy(window, webhookPolicy(scaleURL))

		replicas, _, _, err := Desired(policy, fleetOf(sized(50, 50, 40)), nil, nil, noon, doubling)

		require.NoError(t, err)
		assert.Equal(t, int32(100), replicas)
	})

	t.Run("a schedule without parameters is an error", func(t *testing.T) {
		policy := autoscalingv1.FleetAutoscalerPolicy{Type: scheduleType}

		_, _, _, err := Desired(policy, fleetOf(), nil, nil, noon, neverCalled(t))

		assert.EqualError(t, err, "schedulePolicy parameter must not be nil")
	})
}
