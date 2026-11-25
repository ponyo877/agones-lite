package fleetautoscalers

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	autoscalingv1 "agones.dev/agones/pkg/apis/autoscaling/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ponyo877/scrim/pkg/agones"
	"github.com/ponyo877/scrim/pkg/store"
)

// fixture is a Controller on a store that holds fleet-1 (see fleetOf),
// driven by hand instead of Run.
type fixture struct {
	t *testing.T
	*Controller
}

func newFixture(t *testing.T, call Webhook) fixture {
	t.Helper()
	require.NoError(t, agones.Setup())
	s, err := store.Open(filepath.Join(t.TempDir(), "state.db"), store.Kinds())
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	c := NewController(s, nil, call)
	c.now = func() time.Time { return noon }
	_, err = c.fleets.Create(fleetOf())
	require.NoError(t, err)
	return fixture{t: t, Controller: c}
}

// addScaler creates the FleetAutoscaler of fleet-1 the way the API does:
// defaults first.
func (f fixture) addScaler(policy autoscalingv1.FleetAutoscalerPolicy) {
	f.t.Helper()
	fas := &autoscalingv1.FleetAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "scaler"},
		Spec:       autoscalingv1.FleetAutoscalerSpec{FleetName: "fleet-1", Policy: policy},
	}
	fas.ApplyDefaults()
	_, err := f.scalers.Create(fas)
	require.NoError(f.t, err)
}

func (f fixture) sync() {
	f.t.Helper()
	require.NoError(f.t, f.Controller.sync(context.Background(), store.Key{Kind: store.FleetAutoscalers, Namespace: "default", Name: "scaler"}))
}

func (f fixture) fleet() *agonesv1.Fleet {
	f.t.Helper()
	fleet, ok := f.fleets.Get("default", "fleet-1")
	require.True(f.t, ok, "the Fleet should exist")
	return fleet
}

func (f fixture) scaler() *autoscalingv1.FleetAutoscaler {
	f.t.Helper()
	fas, ok := f.scalers.Get("default", "scaler")
	require.True(f.t, ok, "the FleetAutoscaler should exist")
	return fas
}

func TestFleetAutoscalerResizesItsFleet(t *testing.T) {
	f := newFixture(t, nil)
	f.addScaler(bufferPolicy(amount(5), 1, 20))

	f.sync()

	assert.Equal(t, int32(7), f.fleet().Spec.Replicas, "the 2 Allocated and a buffer of 5")
	status := f.scaler().Status
	assert.True(t, status.AbleToScale)
	assert.Equal(t, int32(7), status.DesiredReplicas)
	assert.Equal(t, noon, status.LastScaleTime.UTC())
}

func TestDeletedFleetAutoscalerIsRemovedAndScalesNoMore(t *testing.T) {
	f := newFixture(t, nil)
	f.addScaler(bufferPolicy(amount(5), 1, 20))
	require.NoError(t, f.scalers.MarkDeleted("default", "scaler", noon))

	f.sync()

	_, ok := f.scalers.Get("default", "scaler")
	assert.False(t, ok, "it has nothing to clean up, so it goes at once")
	assert.Equal(t, int32(8), f.fleet().Spec.Replicas, "the Fleet keeps the size it had")
}

func TestSizeBelowZeroIsNeverWrittenToTheFleet(t *testing.T) {
	tests := map[string]struct {
		policy autoscalingv1.FleetAutoscalerPolicy
		call   Webhook
	}{
		"a webhook asks for one": {
			policy: webhookPolicy(scaleURL),
			call: answering(&autoscalingv1.FleetAutoscaleReview{
				Response: &autoscalingv1.FleetAutoscaleResponse{Scale: true, Replicas: -5},
			}, nil),
		},
		// Admission refuses a minimum below zero. One that is stored all
		// the same must not get any further.
		"a policy that should not exist works one out": {
			policy: bufferPolicy(amount(-10), -5, 20),
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, tc.call)
			f.addScaler(tc.policy)

			f.sync()

			assert.Equal(t, int32(8), f.fleet().Spec.Replicas, "the Fleet keeps the size it had")
			assert.False(t, f.scaler().Status.AbleToScale)
		})
	}
}
