package gameserverallocations

import (
	"maps"
	"slices"
	"time"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	allocationv1 "agones.dev/agones/pkg/apis/allocation/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ponyo877/agones-lite/pkg/gameservers"
)

// maxListCapacity is the largest capacity a list action may set.
// Upstream: the default of the allocator's max-list-items flag
// (cmd/allocator/main.go).
const maxListCapacity = 1000

// Apply returns a copy of gs as it is once gsa has allocated it: state
// Allocated, the request's labels and annotations, its counter and list
// actions, and the last-allocated annotation. Failed counter and list
// actions do not stop an allocation upstream, so they are returned alongside.
//
// Upstream: applyAllocationToGameServer in
// pkg/gameserverallocations/allocator.go, up to the point where it updates
// the GameServer.
func Apply(gsa *allocationv1.GameServerAllocation, gs *agonesv1.GameServer, now time.Time) (*agonesv1.GameServer, []error) {
	allocated := gs.DeepCopy()
	patchMetadata(allocated, gsa.Spec.MetaPatch)
	stampLastAllocated(allocated, now)
	allocated.Status.State = agonesv1.GameServerStateAllocated
	failed := slices.Concat(
		counterActions(gsa.Spec.Counters, allocated),
		listActions(gsa.Spec.Lists, allocated),
	)
	return allocated, failed
}

func patchMetadata(gs *agonesv1.GameServer, patch allocationv1.MetaPatch) {
	for key, value := range patch.Labels {
		metav1.SetMetaDataLabel(&gs.ObjectMeta, key, value)
	}
	for key, value := range patch.Annotations {
		metav1.SetMetaDataAnnotation(&gs.ObjectMeta, key, value)
	}
}

// stampLastAllocated runs after the request's own annotations, so the request
// cannot set this one. A GameServer that is allocated again gets a new time.
func stampLastAllocated(gs *agonesv1.GameServer, now time.Time) {
	stamp, _ := now.UTC().MarshalText() // fails only for a year beyond 9999
	metav1.SetMetaDataAnnotation(&gs.ObjectMeta, gameservers.LastAllocatedAnnotation, string(stamp))
}

// counterActions changes gs's counters and returns the actions that failed.
// Upstream walks the map in Go's random order; sorting the names keeps the
// order of the errors the same from one call to the next.
func counterActions(actions map[string]allocationv1.CounterAction, gs *agonesv1.GameServer) []error {
	var failed []error
	for _, name := range slices.Sorted(maps.Keys(actions)) {
		action := actions[name]
		if err := action.CounterActions(name, gs); err != nil {
			failed = append(failed, err)
		}
	}
	return failed
}

// listActions is counterActions for lists.
func listActions(actions map[string]allocationv1.ListAction, gs *agonesv1.GameServer) []error {
	var failed []error
	for _, name := range slices.Sorted(maps.Keys(actions)) {
		action := actions[name]
		if err := action.ListActions(name, gs, maxListCapacity); err != nil {
			failed = append(failed, err)
		}
	}
	return failed
}
