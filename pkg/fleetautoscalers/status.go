package fleetautoscalers

import (
	"strings"
	"time"

	autoscalingv1 "agones.dev/agones/pkg/apis/autoscaling/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Status returns the FleetAutoscaler's status after a pass in which Desired
// had an answer. current is the Fleet's status.replicas; desired, limited
// and applied are what Desired returned; scaled says whether desired differs
// from the Fleet's spec.replicas, so that the Fleet is resized.
//
// Upstream: updateStatus in pkg/fleetautoscalers/controller.go.
func Status(fas *autoscalingv1.FleetAutoscaler, current, desired int32, scaled, limited bool, applied autoscalingv1.FleetAutoscalerPolicyType, now time.Time) autoscalingv1.FleetAutoscalerStatus {
	status := autoscalingv1.FleetAutoscalerStatus{
		CurrentReplicas:   current,
		DesiredReplicas:   desired,
		LastScaleTime:     fas.Status.LastScaleTime.DeepCopy(),
		AbleToScale:       true,
		ScalingLimited:    limited,
		LastAppliedPolicy: lastApplied(fas, applied),
	}
	if scaled {
		status.LastScaleTime = &metav1.Time{Time: now}
	}
	return status
}

// StatusUnableToScale returns the FleetAutoscaler's status after a pass
// without an answer: the Fleet is missing, or Desired failed. Only the time
// of the last scaling is kept.
//
// Upstream: updateStatusUnableToScale in pkg/fleetautoscalers/controller.go.
func StatusUnableToScale(fas *autoscalingv1.FleetAutoscaler) autoscalingv1.FleetAutoscalerStatus {
	return autoscalingv1.FleetAutoscalerStatus{
		AbleToScale:   false,
		LastScaleTime: fas.Status.LastScaleTime.DeepCopy(),
	}
}

// lastApplied is what status.lastAppliedPolicy becomes: the policy's type,
// or for a Chain the entry that applied.
func lastApplied(fas *autoscalingv1.FleetAutoscaler, applied autoscalingv1.FleetAutoscalerPolicyType) autoscalingv1.FleetAutoscalerPolicyType {
	previous := fas.Status.LastAppliedPolicy
	switch {
	case fas.Spec.Policy.Type != autoscalingv1.ChainPolicyType:
		return fas.Spec.Policy.Type
	case sameEntry(previous, applied):
		return previous
	}
	return applied
}

// sameEntry reports whether upstream takes two values of
// status.lastAppliedPolicy for the same Chain entry, and so leaves the
// status as it was. It splits both at ":" and compares the second and third
// parts, which are the ID and the type unless the ID has a ":" of its own.
//
// Upstream: the end of applyChainPolicy.
func sameEntry(previous, applied autoscalingv1.FleetAutoscalerPolicyType) bool {
	if !strings.Contains(string(previous), string(autoscalingv1.ChainPolicyType)) {
		return false
	}
	was := strings.Split(string(previous), ":")
	is := strings.Split(string(applied), ":")
	if len(was) < 3 || len(is) < 3 {
		return true
	}
	return was[1] == is[1] && was[2] == is[2]
}
