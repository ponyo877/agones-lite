package sdkserver

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"agones.dev/agones/pkg/sdk"

	sidecarv1 "github.com/ponyo877/scrim/proto/sidecar/v1"
)

// health counts missed Health pings. The rule is upstream's checkHealth: a
// check fails when no ping came within one period, and any ping resets the
// count.
type health struct {
	mu       sync.Mutex
	lastPing time.Time
	failures int32
}

// touch records a ping.
func (h *health) touch(now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lastPing = now
	h.failures = 0
}

// check runs one periodic check and returns how many have failed in a row.
func (h *health) check(now time.Time, period time.Duration) int32 {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.lastPing.Add(period).Before(now) {
		h.failures++
	}
	return h.failures
}

// watchHealth reports the game server Unhealthy once it misses too many
// pings in a row.
//
// Upstream starts checking when the kubelet first probes the sidecar. There
// is no kubelet here, so checking starts once initialDelaySeconds have passed.
func (s *Server) watchHealth(ctx context.Context) {
	gs, err := s.get(ctx)
	if err != nil {
		return
	}
	rules := gs.GetSpec().GetHealth()
	if rules.GetDisabled() {
		return
	}
	sleep(ctx, time.Duration(rules.GetInitialDelaySeconds())*time.Second)
	s.health.touch(time.Now())

	period := time.Duration(rules.GetPeriodSeconds()) * time.Second
	tick := time.NewTicker(period)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			if s.failed(now, period, rules) {
				return
			}
		}
	}
}

// failed runs one check and reports the game server when it is over the limit.
func (s *Server) failed(now time.Time, period time.Duration, rules *sdk.GameServer_Spec_Health) bool {
	if s.health.check(now, period) < rules.GetFailureThreshold() {
		return false
	}
	slog.Warn("the game server stopped sending health pings")
	_, _ = s.tell(&sidecarv1.TellRequest{Event: sidecarv1.Event_EVENT_HEALTH_FAILED})
	return true
}
