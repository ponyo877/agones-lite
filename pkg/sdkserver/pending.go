package sdkserver

import (
	"slices"

	"agones.dev/agones/pkg/sdk"
	"google.golang.org/protobuf/proto"

	sidecarv1 "github.com/ponyo877/agones-lite/proto/sidecar/v1"
)

// WithPending returns base as it will be once the control plane has made
// every pending change to its Counters and Lists. base is left untouched.
//
// The control plane makes the same changes for real with ApplyCounts in
// pkg/gameservers, and the two must agree. WithPending is exported so that
// the tests over there can check that they do.
func WithPending(base *sdk.GameServer, pending []*sidecarv1.TellRequest) *sdk.GameServer {
	if len(pending) == 0 {
		return base
	}
	view := proto.Clone(base).(*sdk.GameServer)
	for _, change := range pending {
		for _, update := range change.GetCounters() {
			applyCounter(view.GetStatus(), update)
		}
		for _, update := range change.GetLists() {
			applyList(view.GetStatus(), update)
		}
	}
	return view
}

// applyCounter sets the capacity, sets the count, adds the difference, and
// then cuts the count to fit. A Counter that is gone is skipped.
// Upstream: updateCounter in pkg/sdkserver/sdkserver.go.
func applyCounter(status *sdk.GameServer_Status, update *sidecarv1.CounterUpdate) {
	counter, ok := status.GetCounters()[update.GetName()]
	if !ok {
		return
	}
	counter.Capacity = capacityAfter(counter.GetCapacity(), update.Capacity)
	if update.Count != nil {
		counter.Count = update.GetCount()
	}
	counter.Count = min(max(counter.GetCount()+update.GetCountDiff(), 0), counter.GetCapacity())
}

// applyList sets the capacity, adds values, removes values, and then cuts
// the values to fit. A List that is gone is skipped.
// Upstream: updateList in pkg/sdkserver/sdkserver.go.
func applyList(status *sdk.GameServer_Status, update *sidecarv1.ListUpdate) {
	list, ok := status.GetLists()[update.GetName()]
	if !ok {
		return
	}
	list.Capacity = capacityAfter(list.GetCapacity(), update.Capacity)
	if len(update.GetAdd()) > 0 {
		list.Values = merged(list.GetValues(), update.GetAdd())
	}
	if len(update.GetRemove()) > 0 {
		list.Values = without(list.GetValues(), update.GetRemove())
	}
	// Cutting comes last, so that the values of a full List can be replaced:
	// the new ones are added before the old ones are removed.
	if room := max(list.GetCapacity(), 0); int64(len(list.GetValues())) > room {
		list.Values = slices.Clone(list.GetValues()[:room])
	}
}

// capacityAfter returns the capacity a change asks for, or the current one.
// The SDK calls refuse a negative capacity, so one can only come from a
// broken sender. It is ignored: applying a change never fails.
func capacityAfter(current int64, asked *int64) int64 {
	if asked == nil || *asked < 0 {
		return current
	}
	return *asked
}

// merged returns the distinct values of a, then the new ones of b, in order.
// Upstream: MergeRemoveDuplicates in pkg/apis/agones/v1/gameserver.go, which
// the sidecar cannot import without the Kubernetes types.
func merged(a, b []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, value := range slices.Concat(a, b) {
		if !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	return out
}

// without returns values minus every value in remove.
// Upstream: deleteValues in pkg/sdkserver/sdkserver.go.
func without(values, remove []string) []string {
	gone := map[string]bool{}
	for _, value := range remove {
		gone[value] = true
	}
	out := []string{}
	for _, value := range values {
		if !gone[value] {
			out = append(out, value)
		}
	}
	return out
}
