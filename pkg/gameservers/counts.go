package gameservers

import (
	"math"
	"slices"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
)

// CounterUpdate changes one Counter: capacity first, then count, then the
// difference. It mirrors the message of the same name in proto/sidecar/v1.
type CounterUpdate struct {
	Name      string
	Capacity  *int64 // the new capacity, when set
	Count     *int64 // the new count, when set
	CountDiff int64  // added to the count; may be negative
}

// ListUpdate changes one List: capacity first, then additions, then
// removals. It mirrors the message of the same name in proto/sidecar/v1.
type ListUpdate struct {
	Name     string
	Capacity *int64 // the new capacity, when set
	Add      []string
	Remove   []string
}

// ApplyCounts changes the Counters and Lists in gs.Status as the updates
// say, in order.
//
// The sidecar has already judged each update against its own view and
// answered the game server, so nothing here can fail. An update that no
// longer fits, because the GameServer changed in the meantime, is cut to
// fit; one for a Counter or List that is gone is skipped. Upstream's workers
// do the same: updateCounter and updateList in pkg/sdkserver/sdkserver.go.
//
// The sidecar predicts the outcome with WithPending in pkg/sdkserver, so
// the two must agree.
func ApplyCounts(gs *agonesv1.GameServer, counters []CounterUpdate, lists []ListUpdate) {
	for _, update := range counters {
		if counter, ok := gs.Status.Counters[update.Name]; ok {
			gs.Status.Counters[update.Name] = counterAfter(counter, update)
		}
	}
	for _, update := range lists {
		if list, ok := gs.Status.Lists[update.Name]; ok {
			gs.Status.Lists[update.Name] = listAfter(list, update)
		}
	}
}

func counterAfter(counter agonesv1.CounterStatus, update CounterUpdate) agonesv1.CounterStatus {
	counter.Capacity = capacityAfter(counter.Capacity, update.Capacity)
	if update.Count != nil {
		counter.Count = *update.Count
	}
	counter.Count = min(max(plus(counter.Count, update.CountDiff), 0), counter.Capacity)
	return counter
}

// plus adds without wrapping around: a sum that an int64 cannot hold is the
// largest int64, or the smallest. The count is cut to fit its capacity
// right after, so that is as good as the true sum.
func plus(a, b int64) int64 {
	sum := a + b
	switch {
	case a > 0 && b > 0 && sum < 0:
		return math.MaxInt64
	case a < 0 && b < 0 && sum >= 0:
		return math.MinInt64
	}
	return sum
}

// listAfter does not use the GameServer's own methods (UpdateListCapacity,
// AppendListValues): they cut the values after every step, and the SDK
// server's worker, which this follows, cuts them once at the end.
func listAfter(list agonesv1.ListStatus, update ListUpdate) agonesv1.ListStatus {
	list.Capacity = capacityAfter(list.Capacity, update.Capacity)
	if len(update.Add) > 0 {
		list.Values = agonesv1.MergeRemoveDuplicates(list.Values, update.Add)
	}
	if len(update.Remove) > 0 {
		list.Values = valuesWithout(list.Values, update.Remove)
	}
	// Cutting comes last, so that the values of a full List can be replaced:
	// the new ones are added before the old ones are removed.
	if room := max(list.Capacity, 0); int64(len(list.Values)) > room {
		list.Values = slices.Clone(list.Values[:room])
	}
	return list
}

// capacityAfter returns the capacity an update asks for, or the current one.
// The sidecar refuses a negative capacity, so one can only come from a
// broken sender; it is ignored.
func capacityAfter(current int64, asked *int64) int64 {
	if asked == nil || *asked < 0 {
		return current
	}
	return *asked
}

// valuesWithout returns values minus every value in remove.
func valuesWithout(values, remove []string) []string {
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
