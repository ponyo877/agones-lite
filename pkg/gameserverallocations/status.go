package gameserverallocations

import (
	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	allocationv1 "agones.dev/agones/pkg/apis/allocation/v1"
)

// localSource is the status source of an allocation that was not forwarded
// to another cluster. Upstream: localAllocationSource.
const localSource = "local"

// Allocated returns a copy of gsa answered with gs, the GameServer it was
// given, as stored after Apply. Like upstream, the answer takes the
// GameServer's name.
//
// Upstream: the end of allocateFromLocalCluster in
// pkg/gameserverallocations/allocator.go.
func Allocated(gsa *allocationv1.GameServerAllocation, gs *agonesv1.GameServer) *allocationv1.GameServerAllocation {
	answer := gsa.DeepCopy()
	gs = gs.DeepCopy() // the answer must not share maps and slices with gs
	answer.Name = gs.Name
	answer.Status = allocationv1.GameServerAllocationStatus{
		State:          allocationv1.GameServerAllocationAllocated,
		GameServerName: gs.Name,
		Ports:          gs.Status.Ports,
		Address:        gs.Status.Address,
		Addresses:      gs.Status.Addresses,
		NodeName:       gs.Status.NodeName,
		Source:         localSource,
		Metadata:       &allocationv1.GameServerMetadata{Labels: gs.Labels, Annotations: gs.Annotations},
		Counters:       gs.Status.Counters,
		Lists:          gs.Status.Lists,
	}
	return answer
}

// Unallocated returns a copy of gsa answered with "no GameServer matched".
// Upstream sets the state and nothing else.
func Unallocated(gsa *allocationv1.GameServerAllocation) *allocationv1.GameServerAllocation {
	answer := gsa.DeepCopy()
	answer.Status.State = allocationv1.GameServerAllocationUnAllocated
	return answer
}
