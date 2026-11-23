package fleetautoscalers

import (
	"fmt"
	"testing"
	"time"

	"agones.dev/agones/pkg/apis"
	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	autoscalingv1 "agones.dev/agones/pkg/apis/autoscaling/v1"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

const (
	ready     = agonesv1.GameServerStateReady
	reserved  = agonesv1.GameServerStateReserved
	allocated = agonesv1.GameServerStateAllocated

	bufferType   = autoscalingv1.BufferPolicyType
	webhookType  = autoscalingv1.WebhookPolicyType
	counterType  = autoscalingv1.CounterPolicyType
	listType     = autoscalingv1.ListPolicyType
	scheduleType = autoscalingv1.SchedulePolicyType
	chainType    = autoscalingv1.ChainPolicyType

	// The Counter and the List that the Fleets of these tests have.
	rooms   = "rooms"
	players = "players"
)

var noon = time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)

// at parses a time the way a manifest gives it.
func at(rfc3339 string) time.Time {
	t, err := time.Parse(time.RFC3339, rfc3339)
	if err != nil {
		panic(err)
	}
	return t
}

// amount is a buffer given as a number, share one given in percent.
func amount(n int32) intstr.IntOrString    { return intstr.FromInt32(n) }
func share(text string) intstr.IntOrString { return intstr.FromString(text) }

// fleetOf returns the Fleet of upstream's tests (defaultFixtures): Packed,
// 8 replicas wanted, 5 there, 2 of them Allocated.
func fleetOf(changes ...func(*agonesv1.Fleet)) *agonesv1.Fleet {
	fleet := &agonesv1.Fleet{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "fleet-1"},
		Spec:       agonesv1.FleetSpec{Replicas: 8, Scheduling: apis.Packed},
		Status:     agonesv1.FleetStatus{Replicas: 5, ReadyReplicas: 3, ReservedReplicas: 3, AllocatedReplicas: 2},
	}
	for _, change := range changes {
		change(fleet)
	}
	return fleet
}

// sized sets how many replicas the Fleet's spec asks for, how many its
// status reports, and how many of those are Allocated.
func sized(spec, status, allocated int32) func(*agonesv1.Fleet) {
	return func(fleet *agonesv1.Fleet) {
		fleet.Spec.Replicas = spec
		fleet.Status.Replicas = status
		fleet.Status.AllocatedReplicas = allocated
	}
}

// removing makes scaling down start with the GameServers that have the most
// (Descending) or the least (Ascending) room left in what the policy
// watches. An empty order leaves the Fleet without priorities.
func removing(kind, key, order string) func(*agonesv1.Fleet) {
	return func(fleet *agonesv1.Fleet) {
		if order != "" {
			fleet.Spec.Priorities = []agonesv1.Priority{{Type: kind, Key: key, Order: order}}
		}
	}
}

// option changes one thing about a GameServer built by server.
type option func(*agonesv1.GameServer)

func server(name string, state agonesv1.GameServerState, opts ...option) *agonesv1.GameServer {
	gs := &agonesv1.GameServer{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name}}
	gs.Status.State = state
	for _, opt := range opts {
		opt(gs)
	}
	return gs
}

func on(node string) option {
	return func(gs *agonesv1.GameServer) { gs.Status.NodeName = node }
}

// counting gives the GameServer the rooms Counter.
func counting(count, capacity int64) option {
	return func(gs *agonesv1.GameServer) {
		gs.Status.Counters = map[string]agonesv1.CounterStatus{rooms: {Count: count, Capacity: capacity}}
	}
}

// listing gives the GameServer the players List, holding that many values.
func listing(held, capacity int64) option {
	return func(gs *agonesv1.GameServer) {
		gs.Status.Lists = map[string]agonesv1.ListStatus{players: {Values: values(held), Capacity: capacity}}
	}
}

func values(n int64) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("player-%d", i)
	}
	return out
}

// readyOn returns one Ready GameServer on node for each usage, named after
// the node and numbered in the order given. with is counting or listing.
func readyOn(node string, with func(count, capacity int64) option, each ...usage) []*agonesv1.GameServer {
	servers := make([]*agonesv1.GameServer, len(each))
	for i, u := range each {
		servers[i] = server(fmt.Sprintf("%s-%d", node, i+1), ready, on(node), with(u.count, u.capacity))
	}
	return servers
}

// bufferPolicy keeps size replicas free, between min and max replicas.
func bufferPolicy(size intstr.IntOrString, min, max int32) autoscalingv1.FleetAutoscalerPolicy {
	return autoscalingv1.FleetAutoscalerPolicy{
		Type:   bufferType,
		Buffer: &autoscalingv1.BufferPolicy{BufferSize: size, MinReplicas: min, MaxReplicas: max},
	}
}

// counterPolicy keeps size of the rooms Counter free, between min and max
// total capacity.
func counterPolicy(size intstr.IntOrString, min, max int64) autoscalingv1.FleetAutoscalerPolicy {
	return autoscalingv1.FleetAutoscalerPolicy{
		Type:    counterType,
		Counter: &autoscalingv1.CounterPolicy{Key: rooms, BufferSize: size, MinCapacity: min, MaxCapacity: max},
	}
}

// listPolicy keeps size of the players List free, between min and max total
// capacity.
func listPolicy(size intstr.IntOrString, min, max int64) autoscalingv1.FleetAutoscalerPolicy {
	return autoscalingv1.FleetAutoscalerPolicy{
		Type: listType,
		List: &autoscalingv1.ListPolicy{Key: players, BufferSize: size, MinCapacity: min, MaxCapacity: max},
	}
}

func webhookPolicy(url string) autoscalingv1.FleetAutoscalerPolicy {
	return autoscalingv1.FleetAutoscalerPolicy{Type: webhookType, Webhook: &autoscalingv1.URLConfiguration{URL: &url}}
}

// schedulePolicy applies inner while the schedule is active.
func schedulePolicy(s autoscalingv1.SchedulePolicy, inner autoscalingv1.FleetAutoscalerPolicy) autoscalingv1.FleetAutoscalerPolicy {
	s.Policy = inner
	return autoscalingv1.FleetAutoscalerPolicy{Type: scheduleType, Schedule: &s}
}

func chainPolicy(entries ...autoscalingv1.ChainEntry) autoscalingv1.FleetAutoscalerPolicy {
	return autoscalingv1.FleetAutoscalerPolicy{Type: chainType, Chain: entries}
}

func entry(id string, policy autoscalingv1.FleetAutoscalerPolicy) autoscalingv1.ChainEntry {
	return autoscalingv1.ChainEntry{ID: id, FleetAutoscalerPolicy: policy}
}

// between is a schedule that is active from start to end. Either may be
// empty, for no limit on that side.
func between(start, end string) autoscalingv1.SchedulePolicy {
	var s autoscalingv1.SchedulePolicy
	if start != "" {
		s.Between.Start = metav1.NewTime(at(start))
	}
	if end != "" {
		s.Between.End = metav1.NewTime(at(end))
	}
	return s
}

// neverCalled is the Webhook for policies that must not get as far as one.
func neverCalled(t *testing.T) Webhook {
	return func(*autoscalingv1.URLConfiguration, *autoscalingv1.FleetAutoscaleReview) (*autoscalingv1.FleetAutoscaleReview, error) {
		t.Helper()
		require.FailNow(t, "the webhook must not be called")
		return nil, nil
	}
}
