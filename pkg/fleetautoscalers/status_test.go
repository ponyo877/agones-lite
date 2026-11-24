package fleetautoscalers

import (
	"testing"
	"time"

	autoscalingv1 "agones.dev/agones/pkg/apis/autoscaling/v1"
	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// fasOf returns a FleetAutoscaler with a policy and the status of its last
// pass.
func fasOf(policy autoscalingv1.FleetAutoscalerPolicy, status autoscalingv1.FleetAutoscalerStatus) *autoscalingv1.FleetAutoscaler {
	return &autoscalingv1.FleetAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "fas-1"},
		Spec:       autoscalingv1.FleetAutoscalerSpec{FleetName: "fleet-1", Policy: policy},
		Status:     status,
	}
}

func timeOf(t time.Time) *metav1.Time { return &metav1.Time{Time: t} }

// Upstream: TestControllerUpdateStatus, and what TestControllerSyncFleetAutoscaler
// expects of the status.
func TestStatusAfterAPassWithAnAnswer(t *testing.T) {
	lastNight := noon.Add(-12 * time.Hour)
	tests := map[string]struct {
		before           autoscalingv1.FleetAutoscalerStatus
		current, desired int32
		scaled, limited  bool
		want             autoscalingv1.FleetAutoscalerStatus
	}{
		"the first pass fills in an empty status": {
			current: 5, desired: 12, scaled: true,
			want: autoscalingv1.FleetAutoscalerStatus{
				AbleToScale: true, CurrentReplicas: 5, DesiredReplicas: 12,
				LastScaleTime: timeOf(noon), LastAppliedPolicy: bufferType,
			},
		},
		"a first pass that scales nothing has no time of scaling to tell": {
			current: 10, desired: 10,
			want: autoscalingv1.FleetAutoscalerStatus{
				AbleToScale: true, CurrentReplicas: 10, DesiredReplicas: 10,
				LastAppliedPolicy: bufferType,
			},
		},
		"a pass that finds nothing new returns the status it was given": {
			before: autoscalingv1.FleetAutoscalerStatus{
				AbleToScale: true, CurrentReplicas: 10, DesiredReplicas: 20,
				LastScaleTime: timeOf(lastNight), LastAppliedPolicy: bufferType,
			},
			current: 10, desired: 20,
			want: autoscalingv1.FleetAutoscalerStatus{
				AbleToScale: true, CurrentReplicas: 10, DesiredReplicas: 20,
				LastScaleTime: timeOf(lastNight), LastAppliedPolicy: bufferType,
			},
		},
		"scaling again moves the time of the last scaling": {
			before: autoscalingv1.FleetAutoscalerStatus{
				AbleToScale: true, CurrentReplicas: 10, DesiredReplicas: 20,
				LastScaleTime: timeOf(lastNight), LastAppliedPolicy: bufferType,
			},
			current: 20, desired: 15, scaled: true,
			want: autoscalingv1.FleetAutoscalerStatus{
				AbleToScale: true, CurrentReplicas: 20, DesiredReplicas: 15,
				LastScaleTime: timeOf(noon), LastAppliedPolicy: bufferType,
			},
		},
		"an answer that the policy's limits capped is marked as limited": {
			current: 10, desired: 20, scaled: true, limited: true,
			want: autoscalingv1.FleetAutoscalerStatus{
				AbleToScale: true, ScalingLimited: true, CurrentReplicas: 10, DesiredReplicas: 20,
				LastScaleTime: timeOf(noon), LastAppliedPolicy: bufferType,
			},
		},
		"an answer after a failure makes it able to scale again": {
			before:  autoscalingv1.FleetAutoscalerStatus{LastScaleTime: timeOf(lastNight)},
			current: 5, desired: 5,
			want: autoscalingv1.FleetAutoscalerStatus{
				AbleToScale: true, CurrentReplicas: 5, DesiredReplicas: 5,
				LastScaleTime: timeOf(lastNight), LastAppliedPolicy: bufferType,
			},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			fas := fasOf(bufferPolicy(amount(5), 0, 100), tc.before)

			status := Status(fas, tc.current, tc.desired, tc.scaled, tc.limited, bufferType, noon)

			assert.Equal(t, tc.want, status)
			assert.Equal(t, tc.before, fas.Status, "the FleetAutoscaler itself is left alone")
			if tc.before.LastScaleTime != nil {
				assert.NotSame(t, fas.Status.LastScaleTime, status.LastScaleTime)
			}
		})
	}
}

func TestStatusNamesThePolicyThatApplied(t *testing.T) {
	chain := chainPolicy(fallback)
	tests := map[string]struct {
		policy  autoscalingv1.FleetAutoscalerPolicy
		before  autoscalingv1.FleetAutoscalerPolicyType
		applied autoscalingv1.FleetAutoscalerPolicyType
		want    autoscalingv1.FleetAutoscalerPolicyType
	}{
		"a policy that is not a Chain goes by its type": {
			policy: webhookPolicy(scaleURL), before: "Chain:default:Buffer", applied: webhookType,
			want: webhookType,
		},
		"a Chain goes by the entry that applied": {
			policy: chain, before: "", applied: "Chain:default:Buffer",
			want: "Chain:default:Buffer",
		},
		"another entry takes the place of the last one": {
			policy: chain, before: "Chain:schedule-1:Schedule", applied: "Chain:default:Buffer",
			want: "Chain:default:Buffer",
		},
		"the same ID with another type is another entry": {
			policy: chain, before: "Chain:default:Webhook", applied: "Chain:default:Buffer",
			want: "Chain:default:Buffer",
		},
		// The two below are upstream's doing: it tells entries apart by
		// what stands between the first three colons.
		"an ID with a colon in it can pass for the entry before": {
			policy: chain, before: "Chain:eu:night:Schedule", applied: "Chain:eu:night:late:Buffer",
			want: "Chain:eu:night:Schedule",
		},
		"a status that says Chain without an entry is never corrected": {
			policy: chain, before: "Chain", applied: "Chain:default:Buffer",
			want: "Chain",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			fas := fasOf(tc.policy, autoscalingv1.FleetAutoscalerStatus{LastAppliedPolicy: tc.before})

			status := Status(fas, 5, 5, false, false, tc.applied, noon)

			assert.Equal(t, tc.want, status.LastAppliedPolicy)
		})
	}
}

// Upstream: TestControllerUpdateStatusUnableToScale.
func TestStatusAfterAPassWithoutAnAnswer(t *testing.T) {
	lastNight := timeOf(noon.Add(-12 * time.Hour))
	unable := autoscalingv1.FleetAutoscalerStatus{LastScaleTime: lastNight}
	tests := map[string]autoscalingv1.FleetAutoscalerStatus{
		"everything but the time of the last scaling is forgotten": {
			AbleToScale: true, ScalingLimited: true, CurrentReplicas: 5, DesiredReplicas: 10,
			LastScaleTime: lastNight, LastAppliedPolicy: bufferType,
		},
		"a status that says so already is returned as it was given": unable,
	}
	for name, before := range tests {
		t.Run(name, func(t *testing.T) {
			fas := fasOf(bufferPolicy(amount(5), 0, 100), before)

			status := StatusUnableToScale(fas)

			assert.Equal(t, unable, status)
			assert.NotSame(t, fas.Status.LastScaleTime, status.LastScaleTime)
		})
	}
}
