package sdkserver

import (
	"testing"

	"agones.dev/agones/pkg/sdk"
	"github.com/stretchr/testify/assert"
	"google.golang.org/protobuf/proto"

	sidecarv1 "github.com/ponyo877/agones-lite/proto/sidecar/v1"
)

// counter and list are a Counter and a List in a shape that is easy to
// write down and to compare.
type counter struct{ count, capacity int64 }

type list struct {
	capacity int64
	values   []string
}

// withCounter returns a GameServer at the given version with one Counter,
// "players".
func withCounter(version string, c counter) *sdk.GameServer {
	return &sdk.GameServer{
		ObjectMeta: &sdk.GameServer_ObjectMeta{ResourceVersion: version},
		Status: &sdk.GameServer_Status{Counters: map[string]*sdk.GameServer_Status_CounterStatus{
			"players": {Count: c.count, Capacity: c.capacity},
		}},
	}
}

// withList returns a GameServer at the given version with one List, "rooms".
func withList(version string, l list) *sdk.GameServer {
	return &sdk.GameServer{
		ObjectMeta: &sdk.GameServer_ObjectMeta{ResourceVersion: version},
		Status: &sdk.GameServer_Status{Lists: map[string]*sdk.GameServer_Status_ListStatus{
			"rooms": {Capacity: l.capacity, Values: l.values},
		}},
	}
}

// playersIn and roomsIn read that Counter and that List back.
func playersIn(gs *sdk.GameServer) counter {
	c := gs.GetStatus().GetCounters()["players"]
	return counter{c.GetCount(), c.GetCapacity()}
}

func roomsIn(gs *sdk.GameServer) list {
	l := gs.GetStatus().GetLists()["rooms"]
	return list{l.GetCapacity(), l.GetValues()}
}

func TestPendingCounterChangesAreMadeInOrder(t *testing.T) {
	tests := []struct {
		name    string
		start   counter
		pending []*sidecarv1.CounterUpdate
		want    counter
	}{
		{
			name:    "a difference is added to the count",
			start:   counter{10, 100},
			pending: []*sidecarv1.CounterUpdate{{Name: "players", CountDiff: 9}},
			want:    counter{19, 100},
		},
		{
			name:    "a negative difference is taken off the count",
			start:   counter{10, 100},
			pending: []*sidecarv1.CounterUpdate{{Name: "players", CountDiff: -1}},
			want:    counter{9, 100},
		},
		{
			name:    "the count is set",
			start:   counter{10, 100},
			pending: []*sidecarv1.CounterUpdate{{Name: "players", Count: proto.Int64(42)}},
			want:    counter{42, 100},
		},
		{
			name:    "a smaller capacity takes the count down with it",
			start:   counter{10, 100},
			pending: []*sidecarv1.CounterUpdate{{Name: "players", Capacity: proto.Int64(0)}},
			want:    counter{0, 0},
		},
		{
			name:    "capacity comes first, then count, then the difference",
			start:   counter{10, 100},
			pending: []*sidecarv1.CounterUpdate{{Name: "players", Capacity: proto.Int64(25), Count: proto.Int64(0), CountDiff: 25}},
			want:    counter{25, 25},
		},
		{
			name:    "a count pushed past the capacity stops there",
			start:   counter{10, 12},
			pending: []*sidecarv1.CounterUpdate{{Name: "players", CountDiff: 5}},
			want:    counter{12, 12},
		},
		{
			name:    "a count pushed below zero stops there",
			start:   counter{10, 100},
			pending: []*sidecarv1.CounterUpdate{{Name: "players", CountDiff: -11}},
			want:    counter{0, 100},
		},
		{
			name:  "each change builds on the one before",
			start: counter{10, 100},
			pending: []*sidecarv1.CounterUpdate{
				{Name: "players", Capacity: proto.Int64(5)},
				{Name: "players", CountDiff: -3},
				{Name: "players", Capacity: proto.Int64(50)},
				{Name: "players", CountDiff: 10},
			},
			want: counter{12, 50},
		},
		{
			name:    "a change to a Counter that is gone is skipped",
			start:   counter{10, 100},
			pending: []*sidecarv1.CounterUpdate{{Name: "gone", CountDiff: 1}},
			want:    counter{10, 100},
		},
		{
			name:    "a negative capacity is ignored",
			start:   counter{10, 100},
			pending: []*sidecarv1.CounterUpdate{{Name: "players", Capacity: proto.Int64(-1), CountDiff: 1}},
			want:    counter{11, 100},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var pending []*sidecarv1.TellRequest
			for _, update := range tc.pending {
				pending = append(pending, counterRequest(update))
			}
			view := WithPending(withCounter("1", tc.start), pending)
			assert.Equal(t, tc.want, playersIn(view))
		})
	}
}

func TestPendingListChangesAreMadeInOrder(t *testing.T) {
	tests := []struct {
		name    string
		start   list
		pending []*sidecarv1.ListUpdate
		want    list
	}{
		{
			name:    "a value is added at the end",
			start:   list{10, []string{"one", "two"}},
			pending: []*sidecarv1.ListUpdate{{Name: "rooms", Add: []string{"three"}}},
			want:    list{10, []string{"one", "two", "three"}},
		},
		{
			name:    "a value the List already holds is not added again",
			start:   list{10, []string{"one", "two"}},
			pending: []*sidecarv1.ListUpdate{{Name: "rooms", Add: []string{"two", "three", "three"}}},
			want:    list{10, []string{"one", "two", "three"}},
		},
		{
			name:    "values past the capacity are cut off",
			start:   list{3, []string{"one", "two"}},
			pending: []*sidecarv1.ListUpdate{{Name: "rooms", Add: []string{"three", "four"}}},
			want:    list{3, []string{"one", "two", "three"}},
		},
		{
			name:    "a value is removed, and one the List does not hold is ignored",
			start:   list{10, []string{"one", "two", "three"}},
			pending: []*sidecarv1.ListUpdate{{Name: "rooms", Remove: []string{"two", "nine"}}},
			want:    list{10, []string{"one", "three"}},
		},
		{
			name:    "removing every value leaves an empty List",
			start:   list{10, []string{"one", "two"}},
			pending: []*sidecarv1.ListUpdate{{Name: "rooms", Remove: []string{"two", "one"}}},
			want:    list{10, []string{}},
		},
		{
			name:    "a smaller capacity cuts the values",
			start:   list{10, []string{"one", "two", "three"}},
			pending: []*sidecarv1.ListUpdate{{Name: "rooms", Capacity: proto.Int64(1)}},
			want:    list{1, []string{"one"}},
		},
		{
			name:    "a capacity of zero empties the List",
			start:   list{10, []string{"one", "two", "three"}},
			pending: []*sidecarv1.ListUpdate{{Name: "rooms", Capacity: proto.Int64(0)}},
			want:    list{0, []string{}},
		},
		{
			name:  "the values of a full List can be replaced, because cutting comes last",
			start: list{2, []string{"one", "two"}},
			pending: []*sidecarv1.ListUpdate{
				{Name: "rooms", Add: []string{"three", "four"}, Remove: []string{"one", "two"}},
			},
			want: list{2, []string{"three", "four"}},
		},
		{
			name:  "what is removed makes room before the values are cut",
			start: list{10, []string{"one", "two", "three", "four", "five"}},
			pending: []*sidecarv1.ListUpdate{
				{Name: "rooms", Capacity: proto.Int64(3), Remove: []string{"one"}},
			},
			want: list{3, []string{"two", "three", "four"}},
		},
		{
			name:  "each change builds on the one before",
			start: list{2, []string{"one", "two"}},
			pending: []*sidecarv1.ListUpdate{
				{Name: "rooms", Remove: []string{"one"}},
				{Name: "rooms", Add: []string{"three"}},
				{Name: "rooms", Add: []string{"one"}},
			},
			want: list{2, []string{"two", "three"}},
		},
		{
			name:    "a change to a List that is gone is skipped",
			start:   list{10, []string{"one"}},
			pending: []*sidecarv1.ListUpdate{{Name: "gone", Add: []string{"two"}}},
			want:    list{10, []string{"one"}},
		},
		{
			name:    "a negative capacity is ignored",
			start:   list{10, []string{"one"}},
			pending: []*sidecarv1.ListUpdate{{Name: "rooms", Capacity: proto.Int64(-1), Add: []string{"two"}}},
			want:    list{10, []string{"one", "two"}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var pending []*sidecarv1.TellRequest
			for _, update := range tc.pending {
				pending = append(pending, listRequest(update))
			}
			view := WithPending(withList("1", tc.start), pending)
			assert.Equal(t, tc.want, roomsIn(view))
		})
	}
}

func TestWithPendingLeavesTheBaseAlone(t *testing.T) {
	base := withCounter("1", counter{10, 100})

	assert.Same(t, base, WithPending(base, nil), "with nothing pending the base is the view")

	view := WithPending(base, []*sidecarv1.TellRequest{
		counterRequest(&sidecarv1.CounterUpdate{Name: "players", CountDiff: 1}),
	})
	assert.Equal(t, counter{11, 100}, playersIn(view))
	assert.Equal(t, counter{10, 100}, playersIn(base))
}
