package fleetautoscalers

import (
	"errors"
	"fmt"

	autoscalingv1 "agones.dev/agones/pkg/apis/autoscaling/v1"
)

// chain takes the answer of the first entry that has one. An entry that
// fails, or a Schedule that is not active, passes the turn to the next. When
// no entry is left, the error is that of the last one.
//
// Upstream: applyChainPolicy. It panics on a chain without entries.
func (p pass) chain(entries autoscalingv1.ChainPolicy) (int32, bool, autoscalingv1.FleetAutoscalerPolicyType, error) {
	err := errors.New("chain policy has no entries")
	for _, entry := range entries {
		replicas, limited, failure := p.apply(entry.FleetAutoscalerPolicy)
		if failure == nil {
			return replicas, limited, nameOf(entry), nil
		}
		err = failure
	}
	return 0, false, "", err
}

// nameOf is how upstream writes a Chain entry to status.lastAppliedPolicy.
func nameOf(entry autoscalingv1.ChainEntry) autoscalingv1.FleetAutoscalerPolicyType {
	return autoscalingv1.FleetAutoscalerPolicyType(fmt.Sprintf("%s:%s:%s", autoscalingv1.ChainPolicyType, entry.ID, entry.Type))
}
