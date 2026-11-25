package fleetautoscalers

import (
	"testing"

	autoscalingv1 "agones.dev/agones/pkg/apis/autoscaling/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/validation/field"

	"github.com/ponyo877/scrim/pkg/agones"
)

// Upstream: TestComputeDesiredFleetSize.
func TestDesiredNeedsAPolicyItKnowsAndAFleet(t *testing.T) {
	const wrongType = "wrong policy type, should be one of: Buffer, Webhook, Counter, List, Schedule, Chain"
	buffer := bufferPolicy(amount(20), 0, 100)
	untyped := buffer
	untyped.Type = ""
	tests := map[string]struct {
		policy  autoscalingv1.FleetAutoscalerPolicy
		noFleet bool
		want    string
	}{
		"a policy without a type": {
			policy: untyped,
			want:   wrongType,
		},
		"a type nobody has heard of": {
			policy: autoscalingv1.FleetAutoscalerPolicy{Type: "Hunch"},
			want:   wrongType,
		},
		"Wasm, whose feature gate is off by default": {
			policy: autoscalingv1.FleetAutoscalerPolicy{Type: autoscalingv1.WasmPolicyType, Wasm: &autoscalingv1.WasmPolicy{}},
			want:   wrongType,
		},
		"no Fleet": {
			policy:  buffer,
			noFleet: true,
			want:    "fleet parameter must not be nil",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			fleet := fleetOf()
			if tc.noFleet {
				fleet = nil
			}

			replicas, limited, applied, err := Desired(tc.policy, fleet, nil, nil, noon, neverCalled(t))

			assert.EqualError(t, err, tc.want)
			assert.Zero(t, replicas)
			assert.False(t, limited)
			assert.Empty(t, applied)
		})
	}
}

// Upstream's validation accepts these policies and its controller then
// panics on them. Here they are errors.
func TestPoliciesThatPassUpstreamValidationButCannotBeApplied(t *testing.T) {
	require.NoError(t, agones.Setup())
	always := between("", "")
	tests := map[string]struct {
		policy autoscalingv1.FleetAutoscalerPolicy
		want   string
	}{
		"a chain without entries": {
			policy: chainPolicy(),
			want:   "chain policy has no entries",
		},
		"a schedule whose policy has no parameters": {
			policy: schedulePolicy(always, autoscalingv1.FleetAutoscalerPolicy{Type: bufferType}),
			want:   "bufferPolicy parameter must not be nil",
		},
		"a schedule in a schedule, with a time zone that does not exist": {
			policy: schedulePolicy(always, schedulePolicy(
				scheduleCase{cron: "0 9 * * *", zone: "Mars/Olympus_Mons"}.schedule(),
				bufferPolicy(amount(1), 3, 10),
			)),
			want: "unknown time zone Mars/Olympus_Mons",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			require.Empty(t, tc.policy.ValidatePolicy(field.NewPath("spec", "policy")), "upstream should accept it")

			_, _, _, err := Desired(tc.policy, fleetOf(), nil, nil, noon, neverCalled(t))

			assert.EqualError(t, err, tc.want)
		})
	}
}
