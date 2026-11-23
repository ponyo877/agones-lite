package fleetautoscalers

import (
	"math"
	"testing"

	autoscalingv1 "agones.dev/agones/pkg/apis/autoscaling/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Upstream: TestApplyBufferPolicy.
func TestBufferKeepsReplicasFreeOfPlayers(t *testing.T) {
	tests := map[string]struct {
		allocated int32
		policy    autoscalingv1.FleetAutoscalerPolicy
		want      int32
		limited   bool
	}{
		"a number is added to the Allocated replicas": {
			allocated: 40, policy: bufferPolicy(amount(20), 0, 100),
			want: 60,
		},
		"the minimum is used when the buffer asks for less": {
			allocated: 40, policy: bufferPolicy(amount(20), 65, 100),
			want: 65, limited: true,
		},
		"the maximum is used when the buffer asks for more": {
			allocated: 40, policy: bufferPolicy(amount(20), 0, 55),
			want: 55, limited: true,
		},
		"asking for exactly the maximum is not being limited": {
			allocated: 35, policy: bufferPolicy(amount(20), 0, 55),
			want: 55,
		},
		"a share is taken of the size to come, not of the size now": {
			// 20% free means that the 50 Allocated are the other 80%.
			allocated: 50, policy: bufferPolicy(share("20%"), 0, 100),
			want: 63,
		},
		"a share is rounded up to a whole replica": {
			allocated: 1, policy: bufferPolicy(share("10%"), 0, 10),
			want: 2,
		},
		"a share of nothing is nothing, which leaves the minimum": {
			allocated: 0, policy: bufferPolicy(share("20%"), 1, 10),
			want: 1, limited: true,
		},
		"a minimum above the maximum loses to it": {
			allocated: 40, policy: bufferPolicy(amount(20), 80, 70),
			want: 70, limited: true,
		},
		// Upstream adds the two as int32, and the sum comes out negative.
		"a size too large for a Fleet is the maximum, not a number that wrapped around": {
			allocated: 40, policy: bufferPolicy(amount(math.MaxInt32), 0, math.MaxInt32),
			want: math.MaxInt32, limited: true,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			fleet := fleetOf(sized(50, 50, tc.allocated))

			replicas, limited, applied, err := Desired(tc.policy, fleet, nil, nil, noon, neverCalled(t))

			require.NoError(t, err)
			assert.Equal(t, tc.want, replicas)
			assert.Equal(t, tc.limited, limited)
			assert.Equal(t, bufferType, applied)
		})
	}
}

func TestBufferThatCannotBeReadIsAnError(t *testing.T) {
	tests := map[string]struct {
		policy autoscalingv1.FleetAutoscalerPolicy
		want   string
	}{
		"a text that is not a percentage": {
			policy: bufferPolicy(share("asd"), 0, 10),
			want:   "invalid value for IntOrString: invalid type: string is not a percentage",
		},
		"no buffer parameters at all": {
			policy: autoscalingv1.FleetAutoscalerPolicy{Type: bufferType},
			want:   "bufferPolicy parameter must not be nil",
		},
		// Upstream divides by 100 less the share: by zero here, and by less
		// than zero for the next.
		"a share that leaves nothing for the players": {
			policy: bufferPolicy(share("100%"), 1, 10),
			want:   "bufferSize should be between 1% and 99%, and is 100%",
		},
		"a share of more than there is": {
			policy: bufferPolicy(share("150%"), 1, 10),
			want:   "bufferSize should be between 1% and 99%, and is 150%",
		},
		"a share of nothing": {
			policy: bufferPolicy(share("0%"), 1, 10),
			want:   "bufferSize should be between 1% and 99%, and is 0%",
		},
		// Upstream leaves it to the API server to refuse the Fleet.
		"limits that lead below zero": {
			policy: bufferPolicy(amount(-10), -5, 20),
			want:   "Buffer policy asks for -5 replicas, which is fewer than none",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			_, _, _, err := Desired(tc.policy, fleetOf(), nil, nil, noon, neverCalled(t))

			assert.EqualError(t, err, tc.want)
		})
	}
}
