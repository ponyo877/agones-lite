package fleetautoscalers

import (
	"errors"
	"fmt"
	"time"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	autoscalingv1 "agones.dev/agones/pkg/apis/autoscaling/v1"

	"github.com/ponyo877/agones-lite/pkg/gameservers"
)

// Desired returns how many replicas the policy wants for the Fleet, and
// whether the policy's own limits capped the answer.
//
// servers are the GameServers of the Fleet and counts says how full each
// node is. Only the Counter and List policies look at them, to see what
// scaling down would remove. call is asked when a Webhook policy is reached.
//
// applied names the policy that gave the answer, the way upstream writes it
// to status.lastAppliedPolicy: the policy's type, or for a Chain the entry
// that applied, as "Chain:<id>:<type>". Status wants it back.
//
// Upstream: computeDesiredFleetSize in pkg/fleetautoscalers/fleetautoscalers.go.
func Desired(policy autoscalingv1.FleetAutoscalerPolicy, fleet *agonesv1.Fleet, servers []*agonesv1.GameServer, counts map[string]gameservers.NodeCount, now time.Time, call Webhook) (replicas int32, limited bool, applied autoscalingv1.FleetAutoscalerPolicyType, err error) {
	if fleet == nil {
		return 0, false, "", errors.New("fleet parameter must not be nil")
	}
	p := pass{fleet: fleet, servers: servers, counts: counts, now: now, call: call}
	if policy.Type == autoscalingv1.ChainPolicyType {
		return p.chain(policy.Chain)
	}
	replicas, limited, err = p.apply(policy)
	if err != nil {
		return 0, false, "", err
	}
	return replicas, limited, policy.Type, nil
}

// pass is everything a policy may look at: the arguments of Desired.
type pass struct {
	fleet   *agonesv1.Fleet
	servers []*agonesv1.GameServer
	counts  map[string]gameservers.NodeCount
	now     time.Time
	call    Webhook
}

// apply asks one policy. Schedule and Chain come back here for the policies
// they contain.
//
// A size below zero is no answer. Admission keeps the limits that could
// lead to one out, except from the policy inside a Schedule, and upstream
// leaves it to the API server to refuse the Fleet.
func (p pass) apply(policy autoscalingv1.FleetAutoscalerPolicy) (replicas int32, limited bool, err error) {
	replicas, limited, err = p.ask(policy)
	if err == nil && replicas < 0 {
		return 0, false, fmt.Errorf("%s policy asks for %d replicas, which is fewer than none", policy.Type, replicas)
	}
	return replicas, limited, err
}

// ask hands the policy to the function for its type.
func (p pass) ask(policy autoscalingv1.FleetAutoscalerPolicy) (replicas int32, limited bool, err error) {
	switch policy.Type {
	case autoscalingv1.BufferPolicyType:
		return p.buffer(policy.Buffer)
	case autoscalingv1.WebhookPolicyType:
		return p.webhook(policy.Webhook)
	case autoscalingv1.CounterPolicyType:
		return p.counter(policy.Counter)
	case autoscalingv1.ListPolicyType:
		return p.list(policy.List)
	case autoscalingv1.SchedulePolicyType:
		return p.schedule(policy.Schedule)
	case autoscalingv1.ChainPolicyType:
		replicas, limited, _, err = p.chain(policy.Chain)
		return replicas, limited, err
	}
	return 0, false, errors.New("wrong policy type, should be one of: Buffer, Webhook, Counter, List, Schedule, Chain")
}
