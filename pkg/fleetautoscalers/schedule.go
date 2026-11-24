package fleetautoscalers

import (
	"errors"
	"time"

	autoscalingv1 "agones.dev/agones/pkg/apis/autoscaling/v1"
	"github.com/robfig/cron/v3"
)

// ErrInactiveSchedule is the answer of a Schedule policy outside its active
// time. In a Chain it passes the turn to the next entry, as any error does.
//
// Upstream: InactiveScheduleError. Upstream means to pass over it quietly,
// but it returns a pointer and looks for a value, so its controller treats
// it as a failure like any other.
var ErrInactiveSchedule = errors.New("inactive schedule, policy not applicable")

const (
	// earlyStart is how long before its cron time an active period begins.
	// Upstream adds it for the sake of daylight saving time.
	earlyStart = 90 * time.Second
	// forever stands in for a duration that was left out: 290 years, nearly
	// the longest time.Duration there is. Upstream: maxDuration.
	forever = 2540400 * time.Hour
)

// schedule applies the policy inside s while s is active.
//
// Upstream: applySchedulePolicy.
func (p pass) schedule(s *autoscalingv1.SchedulePolicy) (int32, bool, error) {
	if s == nil {
		return 0, false, errors.New("schedulePolicy parameter must not be nil")
	}
	on, err := active(s, p.now)
	if err != nil {
		return 0, false, err
	}
	if !on {
		return 0, false, ErrInactiveSchedule
	}
	return p.apply(s.Policy)
}

// active reports whether s applies at now. Without a cron expression it
// applies all the time from start to end, whatever the duration says.
//
// Upstream: isScheduleActive.
func active(s *autoscalingv1.SchedulePolicy, now time.Time) (bool, error) {
	if now.Before(s.Between.Start.Time) {
		return false, nil
	}
	if end := s.Between.End.Time; !end.IsZero() && now.After(end) {
		return false, nil
	}
	if s.ActivePeriod.StartCron == "" {
		return true, nil
	}
	return inActivePeriod(s.ActivePeriod, now)
}

// inActivePeriod reports whether now falls in one of the periods that begin
// at a cron time and last for the duration.
//
// Upstream trusts admission and ignores what does not parse. Here it is an
// error.
func inActivePeriod(period autoscalingv1.ActivePeriod, now time.Time) (bool, error) {
	zone, err := time.LoadLocation(period.Timezone)
	if err != nil {
		return false, err
	}
	starts, err := cron.ParseStandard(period.StartCron)
	if err != nil {
		return false, err
	}
	length, err := lengthOf(period)
	if err != nil {
		return false, err
	}
	// A period that covers now began no longer ago than its length.
	start := starts.Next(now.Add(-length).In(zone))
	return now.After(start.Add(-earlyStart)) && now.Before(start.Add(length)), nil
}

func lengthOf(period autoscalingv1.ActivePeriod) (time.Duration, error) {
	if period.Duration == "" {
		return forever, nil
	}
	return time.ParseDuration(period.Duration)
}
