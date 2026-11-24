// Package metrics exposes the state of the control plane to Prometheus,
// under the metric names and labels Agones uses, so dashboards made for
// Agones can be pointed at agones-lite.
//
// Most metrics are gauges that are simply read from the store each time
// Prometheus asks (collector.go). Two things cannot be read back and are
// counted as they happen: how often game servers enter each state, and how
// long allocations take.
//
// One metric is agones-lite's own, and named so: for how long the store's
// file has been behind (agones_lite_store_lag_seconds). It is the one number
// that says whether the control plane can keep its promises.
//
// Upstream counterpart: pkg/metrics and pkg/gameserverallocations/metrics.go.
// Left out are the metrics about the Kubernetes client and about player
// tracking, which have nothing to report here.
package metrics

import (
	"net/http"
	"time"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/ponyo877/agones-lite/pkg/store"
)

// Metrics collects everything agones-lite reports. A nil *Metrics is valid
// and records nothing, which is convenient in tests.
type Metrics struct {
	registry    *prometheus.Registry
	transitions *prometheus.CounterVec
	allocations *prometheus.HistogramVec
}

// finer is how much wider each bucket of the native histogram is than the
// one before it. A quantile read from such buckets is off by a twentieth of
// itself at most, wherever the values fall and without anyone having
// guessed their range beforehand: the guarantee of DDSketch (Masson, Rim
// and Lee, VLDB 2019), which these buckets share.
const finer = 1.1

// New returns Metrics that read their gauges from s.
func New(s *store.Store) *Metrics {
	m := &Metrics{
		registry: prometheus.NewRegistry(),
		transitions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "agones_gameservers_total",
			Help: "The total of gameservers per fleet and status",
		}, []string{"type", "fleet_name", "namespace"}),
		allocations: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "agones_gameserver_allocations_duration_seconds",
			Help: "The distribution of gameserver allocation requests latencies.",
			// Upstream's buckets, for upstream's dashboards. They begin at
			// 10 ms, and an allocation here takes one to five: all of them
			// fall into the first, and a quantile read from these says
			// "under 10 ms" whatever happened. So the same observations go
			// into a native histogram as well, which a Prometheus that asks
			// for it gets beside the classic one.
			Buckets:                         []float64{0, 0.01, 0.05, 0.1, 0.25, 0.5, 1, 2, 3},
			NativeHistogramBucketFactor:     finer,
			NativeHistogramMaxBucketNumber:  160,
			NativeHistogramMinResetDuration: time.Hour,
		}, []string{"fleet_name", "cluster_name", "is_multicluster", "status", "scheduling_strategy"}),
	}
	lag := prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "agones_lite_store_lag_seconds",
		Help: "For how long the file has been behind memory: the age of the oldest change that is not saved yet",
	}, func() float64 { return s.Lag().Seconds() })
	m.registry.MustRegister(m.transitions, m.allocations, lag, newCollector(s))
	return m
}

// Handler serves the metrics in the format Prometheus scrapes.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

// GameServerEntered counts a GameServer entering a state.
func (m *Metrics) GameServerEntered(gs *agonesv1.GameServer, state agonesv1.GameServerState) {
	if m == nil {
		return
	}
	m.transitions.WithLabelValues(string(state), fleetOf(gs), gs.Namespace).Inc()
}

// Allocated records one allocation request: how it ended and how long it took.
// status is "Allocated", "UnAllocated" or "Error", as upstream reports it.
func (m *Metrics) Allocated(fleet, status, strategy string, took time.Duration) {
	if m == nil {
		return
	}
	m.allocations.WithLabelValues(fleet, "", "false", status, strategy).Observe(took.Seconds())
}

// fleetOf returns the Fleet a GameServer belongs to, or upstream's "none".
func fleetOf(gs *agonesv1.GameServer) string {
	if fleet := gs.Labels[agonesv1.FleetNameLabel]; fleet != "" {
		return fleet
	}
	return "none"
}
