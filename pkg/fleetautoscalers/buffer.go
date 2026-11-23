package fleetautoscalers

import (
	"errors"
	"math"

	autoscalingv1 "agones.dev/agones/pkg/apis/autoscaling/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// buffer keeps a number of GameServers, or a share of the Fleet, free of
// players, between a minimum and a maximum size.
//
// Upstream: applyBufferPolicy.
func (p pass) buffer(b *autoscalingv1.BufferPolicy) (int32, bool, error) {
	if b == nil {
		return 0, false, errors.New("bufferPolicy parameter must not be nil")
	}
	size, err := withBuffer(int64(p.fleet.Status.AllocatedReplicas), b.BufferSize)
	if err != nil {
		return 0, false, err
	}
	replicas, limited := int32(size), false
	if replicas < b.MinReplicas {
		replicas, limited = b.MinReplicas, true
	}
	if replicas > b.MaxReplicas {
		replicas, limited = b.MaxReplicas, true
	}
	return replicas, limited, nil
}

// withBuffer returns the size that leaves the buffer free once allocated is
// taken out of it.
//
// A buffer in percent is a share of that size, not of the size now: with
// 30%, the allocated part has to end up as 70% of the result.
func withBuffer(allocated int64, buffer intstr.IntOrString) (int64, error) {
	if buffer.Type == intstr.Int {
		return allocated + int64(buffer.IntValue()), nil
	}
	percent, err := intstr.GetScaledValueFromIntOrPercent(&buffer, 100, true)
	if err != nil {
		return 0, err
	}
	return divideUp(allocated*100, int64(100-percent)), nil
}

// divideUp divides and rounds up, in floating point as upstream does.
func divideUp(a, b int64) int64 {
	return int64(math.Ceil(float64(a) / float64(b)))
}
