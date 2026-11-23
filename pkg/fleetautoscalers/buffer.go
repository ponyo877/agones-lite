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
	// The size is held to the limits before it is cut down to an int32: on
	// its own it may be too large for one, and would wrap around.
	limited := false
	if size < int64(b.MinReplicas) {
		size, limited = int64(b.MinReplicas), true
	}
	if size > int64(b.MaxReplicas) {
		size, limited = int64(b.MaxReplicas), true
	}
	return int32(size), limited, nil
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
	return roundUp(hundredfold(allocated) / float64(100-percent)), nil
}

// hundredfold is n times 100. Upstream multiplies as integers, which wraps
// around past the end of int64: from there on this multiplies in floating
// point.
func hundredfold(n int64) float64 {
	if n > math.MaxInt64/100 {
		return float64(n) * 100
	}
	return float64(n * 100)
}

// divideUp divides and rounds up, in floating point as upstream does.
func divideUp(a, b int64) int64 {
	return roundUp(float64(a) / float64(b))
}

// roundUp is x as a whole number, rounded up. Beyond the end of int64 it is
// the largest int64: what Go converts such an x to depends on the CPU.
func roundUp(x float64) int64 {
	if x >= math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(math.Ceil(x))
}
