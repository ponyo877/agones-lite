package fleetautoscalers

import (
	"errors"
	"fmt"
	"math"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
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
	allocated := int64(p.fleet.Status.AllocatedReplicas)
	free, err := bufferFor(allocated, b.BufferSize)
	if err != nil {
		return 0, false, err
	}
	// The size is held to the limits before it is cut down to an int32: on
	// its own it may be too large for one, and would wrap around. As an
	// int64 it cannot: a Fleet's replicas and 99 times as many fit easily.
	size, limited := allocated+free, false
	if size < int64(b.MinReplicas) {
		size, limited = int64(b.MinReplicas), true
	}
	if size > int64(b.MaxReplicas) {
		size, limited = int64(b.MaxReplicas), true
	}
	return int32(size), limited, nil
}

// bufferFor returns how much has to be free next to what is allocated.
//
// A buffer in percent is a share of the two together, not of the allocated
// part: with 30% free, the allocated part is the other 70%, and the buffer
// is three sevenths of it, rounded up.
//
// Upstream divides in floating point: allocated * 100 / (100 - percent),
// less allocated. Large counts lose their last digits that way. And 100%
// divides by zero: what Go makes of the outcome depends on the CPU, and a
// Counter policy with nothing allocated then shrinks the Fleet on amd64 and
// leaves it alone on arm64. Admission keeps such a share out, except from
// the policy inside a Schedule, which it does not look at. Here every share
// that is not from 1% to 99% is an error, and the rest is whole numbers.
func bufferFor(allocated int64, buffer intstr.IntOrString) (int64, error) {
	if buffer.Type == intstr.Int {
		return int64(buffer.IntValue()), nil
	}
	percent, err := intstr.GetScaledValueFromIntOrPercent(&buffer, 100, true)
	if err != nil {
		return 0, err
	}
	if percent < 1 || percent > 99 {
		return 0, fmt.Errorf("bufferSize should be between 1%% and 99%%, and is %s", buffer.StrVal)
	}
	// A share of nothing is nothing.
	return scaled(max(allocated, 0), int64(percent), int64(100-percent)), nil
}

// scaled is n * times / by, rounded up, for an n that is not negative and
// a times and a by that are above zero. Beyond the end of int64 it is the
// largest int64.
func scaled(n, times, by int64) int64 {
	// n is whole * by + part, so the answer is whole * times, and what the
	// part adds. Multiplying n itself could wrap around.
	whole, part := n/by, n%by
	if whole > math.MaxInt64/times {
		return math.MaxInt64
	}
	return agonesv1.SafeAdd(whole*times, divideUp(part*times, by))
}

// divideUp is a / b rounded up, for a b above zero.
func divideUp(a, b int64) int64 {
	quotient := a / b // rounded toward zero, which for a negative a is up
	if a%b > 0 {
		quotient++
	}
	return quotient
}
