package metrics

import (
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	autoscalingv1 "agones.dev/agones/pkg/apis/autoscaling/v1"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	litev1 "github.com/ponyo877/agones-lite/pkg/apis/lite/v1"
	"github.com/ponyo877/agones-lite/pkg/store"
)

func scrape(t *testing.T, m *Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	return rec.Body.String()
}

func server(name, fleet string, state agonesv1.GameServerState, node string) *agonesv1.GameServer {
	gs := &agonesv1.GameServer{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name}}
	if fleet != "" {
		gs.Labels = map[string]string{agonesv1.FleetNameLabel: fleet}
	}
	gs.Status.State, gs.Status.NodeName = state, node
	return gs
}

func TestGaugesAreReadFromTheStore(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "state.db"), store.Kinds())
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	m := New(s)

	for _, gs := range []*agonesv1.GameServer{
		server("a", "simple", agonesv1.GameServerStateReady, "node-1"),
		server("b", "simple", agonesv1.GameServerStateReady, "node-1"),
		server("c", "simple", agonesv1.GameServerStateAllocated, "node-1"),
		server("d", "", agonesv1.GameServerStateReady, "node-1"),
	} {
		_, err := store.GameServersOf(s).Create(gs)
		require.NoError(t, err)
	}
	for _, name := range []string{"node-1", "node-2"} {
		_, err := store.NodesOf(s).Create(&litev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}})
		require.NoError(t, err)
	}
	fleet := &agonesv1.Fleet{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "simple"}}
	fleet.Spec.Replicas = 5
	fleet.Status = agonesv1.FleetStatus{Replicas: 3, ReadyReplicas: 2, AllocatedReplicas: 1,
		Counters: map[string]agonesv1.AggregatedCounterStatus{"rooms": {Count: 4, Capacity: 30, AllocatedCount: 3, AllocatedCapacity: 10}}}
	_, err = store.FleetsOf(s).Create(fleet)
	require.NoError(t, err)
	fas := &autoscalingv1.FleetAutoscaler{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "scaler"}}
	fas.Spec.FleetName = "simple"
	fas.Status = autoscalingv1.FleetAutoscalerStatus{CurrentReplicas: 3, DesiredReplicas: 5, AbleToScale: true}
	_, err = store.FleetAutoscalersOf(s).Create(fas)
	require.NoError(t, err)

	text := scrape(t, m)

	for _, line := range []string{
		`agones_gameservers_count{fleet_name="simple",namespace="default",type="Ready"} 2`,
		`agones_gameservers_count{fleet_name="simple",namespace="default",type="Allocated"} 1`,
		`agones_gameservers_count{fleet_name="none",namespace="default",type="Ready"} 1`,
		`agones_fleets_replicas_count{name="simple",namespace="default",type="desired"} 5`,
		`agones_fleets_replicas_count{name="simple",namespace="default",type="ready"} 2`,
		`agones_fleet_counters{counter="rooms",name="simple",namespace="default",type="total_capacity"} 30`,
		`agones_fleet_counters{counter="rooms",name="simple",namespace="default",type="allocated_count"} 3`,
		`agones_fleet_autoscalers_desired_replicas_count{fleet_name="simple",name="scaler",namespace="default"} 5`,
		`agones_fleet_autoscalers_able_to_scale{fleet_name="simple",name="scaler",namespace="default"} 1`,
		`agones_fleet_autoscalers_limited{fleet_name="simple",name="scaler",namespace="default"} 0`,
		`agones_nodes_count{empty="false"} 1`,
		`agones_nodes_count{empty="true"} 1`,
	} {
		assert.Contains(t, text, line)
	}
}

func TestEventsAreCountedAsTheyHappen(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "state.db"), store.Kinds())
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	m := New(s)

	gs := server("a", "simple", agonesv1.GameServerStateReady, "node-1")
	m.GameServerEntered(gs, agonesv1.GameServerStateReady)
	m.GameServerEntered(gs, agonesv1.GameServerStateAllocated)
	m.GameServerEntered(gs, agonesv1.GameServerStateReady)
	m.Allocated("simple", "Allocated", "Packed", 20*time.Millisecond)

	text := scrape(t, m)

	assert.Contains(t, text, `agones_gameservers_total{fleet_name="simple",namespace="default",type="Ready"} 2`)
	assert.Contains(t, text, `agones_gameservers_total{fleet_name="simple",namespace="default",type="Allocated"} 1`)
	assert.Contains(t, text, `agones_gameserver_allocations_duration_seconds_count{cluster_name="",fleet_name="simple",is_multicluster="false",scheduling_strategy="Packed",status="Allocated"} 1`)
}

func TestNilMetricsRecordNothing(t *testing.T) {
	var m *Metrics
	m.GameServerEntered(server("a", "", agonesv1.GameServerStateReady, ""), agonesv1.GameServerStateReady)
	m.Allocated("simple", "Allocated", "Packed", time.Millisecond)
}

// Upstream's buckets begin at 10 ms, so of an allocation that took three
// they say no more than that. The native histogram beside them says it to
// within a tenth.
func TestAnAllocationIsMeasuredToWithinATenthOfItself(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "state.db"), store.Kinds())
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	m := New(s)
	const took = 3 * time.Millisecond

	m.Allocated("simple", "Allocated", "Packed", took)

	histogram := allocations(t, m)
	require.Len(t, histogram.GetPositiveSpan(), 1, "one observation, one bucket")
	// A bucket's upper bound is 2^(index / 2^schema); the one before it is
	// its lower bound.
	index := float64(histogram.GetPositiveSpan()[0].GetOffset())
	perDoubling := math.Pow(2, float64(histogram.GetSchema()))
	above, below := math.Pow(2, index/perDoubling), math.Pow(2, (index-1)/perDoubling)
	assert.LessOrEqual(t, above/below, finer)
	assert.Greater(t, took.Seconds(), below)
	assert.LessOrEqual(t, took.Seconds(), above)
	assert.Equal(t, uint64(1), histogram.GetBucket()[1].GetCumulativeCount(), "upstream's buckets have it too, as under 10 ms")
}

// allocations returns the histogram of the one series there is.
func allocations(t *testing.T, m *Metrics) *dto.Histogram {
	t.Helper()
	families, err := m.registry.Gather()
	require.NoError(t, err)
	for _, family := range families {
		if family.GetName() == "agones_gameserver_allocations_duration_seconds" {
			return family.GetMetric()[0].GetHistogram()
		}
	}
	require.Fail(t, "no allocation was measured")
	return nil
}

func TestTheStoresLagIsReported(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "state.db"), store.Kinds())
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	assert.Contains(t, scrape(t, New(s)), "agones_lite_store_lag_seconds 0\n", "the file has everything")
}
