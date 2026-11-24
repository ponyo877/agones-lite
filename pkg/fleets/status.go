package fleets

import (
	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
)

// Status aggregates the sets' status into the Fleet's.
//
// Allocations is carried over from fleet: it is a running total, and
// counting new allocations is the caller's job. Players stays empty because
// it belongs to the PlayerTracking feature gate, which is off by default.
//
// Upstream: updateFleetStatus in pkg/fleets/controller.go.
func Status(fleet *agonesv1.Fleet, sets []*agonesv1.GameServerSet) agonesv1.FleetStatus {
	status := agonesv1.FleetStatus{
		Allocations: fleet.Status.Allocations,
		Counters:    map[string]agonesv1.AggregatedCounterStatus{},
		Lists:       map[string]agonesv1.AggregatedListStatus{},
	}
	for _, set := range sets {
		status.Replicas += set.Status.Replicas
		status.ReadyReplicas += set.Status.ReadyReplicas
		status.ReservedReplicas += set.Status.ReservedReplicas
		status.AllocatedReplicas += set.Status.AllocatedReplicas
		addCounters(status.Counters, set.Status.Counters)
		addLists(status.Lists, set.Status.Lists)
	}
	return status
}

// addCounters adds the Counters of one set to total. A sum stops at the
// largest int64 instead of wrapping around.
//
// Upstream: mergeCounters in pkg/fleets/controller.go.
func addCounters(total, counters map[string]agonesv1.AggregatedCounterStatus) {
	for name, counter := range counters {
		sum := total[name]
		sum.AllocatedCount = agonesv1.SafeAdd(sum.AllocatedCount, counter.AllocatedCount)
		sum.AllocatedCapacity = agonesv1.SafeAdd(sum.AllocatedCapacity, counter.AllocatedCapacity)
		sum.Count = agonesv1.SafeAdd(sum.Count, counter.Count)
		sum.Capacity = agonesv1.SafeAdd(sum.Capacity, counter.Capacity)
		total[name] = sum
	}
}

// addLists adds the Lists of one set to total.
//
// Upstream: mergeLists in pkg/fleets/controller.go.
func addLists(total, lists map[string]agonesv1.AggregatedListStatus) {
	for name, list := range lists {
		sum := total[name]
		sum.AllocatedCount += list.AllocatedCount
		sum.AllocatedCapacity += list.AllocatedCapacity
		sum.Count += list.Count
		sum.Capacity += list.Capacity
		total[name] = sum
	}
}
