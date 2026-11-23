package gameserversets

import (
	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
)

// Status summarises the GameServers of a set.
//
// Upstream: computeStatus in pkg/gameserversets/controller.go. Players stays
// empty because it belongs to the PlayerTracking feature gate, which is off
// by default.
func Status(set *agonesv1.GameServerSet, servers []*agonesv1.GameServer) agonesv1.GameServerSetStatus {
	status := emptyStatus(set)
	for _, gs := range servers {
		if gs.IsBeingDeleted() {
			status.ShutdownReplicas++
			continue
		}
		status.Replicas++
		switch gs.Status.State {
		case agonesv1.GameServerStateReady:
			status.ReadyReplicas++
		case agonesv1.GameServerStateAllocated:
			status.AllocatedReplicas++
		case agonesv1.GameServerStateReserved:
			status.ReservedReplicas++
		}
		addCounters(status.Counters, gs)
		addLists(status.Lists, gs)
	}
	return status
}

// emptyStatus has a zero entry for every Counter and List in the set's
// template, so that they are reported even when no GameServer has them.
func emptyStatus(set *agonesv1.GameServerSet) agonesv1.GameServerSetStatus {
	status := agonesv1.GameServerSetStatus{
		Counters: map[string]agonesv1.AggregatedCounterStatus{},
		Lists:    map[string]agonesv1.AggregatedListStatus{},
	}
	for name := range set.Spec.Template.Spec.Counters {
		status.Counters[name] = agonesv1.AggregatedCounterStatus{}
	}
	for name := range set.Spec.Template.Spec.Lists {
		status.Lists[name] = agonesv1.AggregatedListStatus{}
	}
	return status
}

// addCounters adds the Counters of gs to total.
//
// Upstream: aggregateCounters.
func addCounters(total map[string]agonesv1.AggregatedCounterStatus, gs *agonesv1.GameServer) {
	allocated := gs.Status.State == agonesv1.GameServerStateAllocated
	for name, counter := range gs.Status.Counters {
		sum := total[name]
		sum.Count = agonesv1.SafeAdd(sum.Count, counter.Count)
		sum.Capacity = agonesv1.SafeAdd(sum.Capacity, counter.Capacity)
		if allocated {
			sum.AllocatedCount = agonesv1.SafeAdd(sum.AllocatedCount, counter.Count)
			sum.AllocatedCapacity = agonesv1.SafeAdd(sum.AllocatedCapacity, counter.Capacity)
		}
		total[name] = sum
	}
}

// addLists adds the Lists of gs to total. A value that appears twice is
// counted twice.
//
// Upstream: aggregateLists.
func addLists(total map[string]agonesv1.AggregatedListStatus, gs *agonesv1.GameServer) {
	allocated := gs.Status.State == agonesv1.GameServerStateAllocated
	for name, list := range gs.Status.Lists {
		values := int64(len(list.Values))
		sum := total[name]
		sum.Count += values
		sum.Capacity += list.Capacity
		if allocated {
			sum.AllocatedCount += values
			sum.AllocatedCapacity += list.Capacity
		}
		total[name] = sum
	}
}
