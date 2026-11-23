package gameservers

import (
	"slices"
	"testing"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	"agones.dev/agones/pkg/sdk"
	"github.com/stretchr/testify/assert"
	"google.golang.org/protobuf/proto"

	"github.com/ponyo877/agones-lite/pkg/sdkserver"
	sidecarv1 "github.com/ponyo877/agones-lite/proto/sidecar/v1"
)

func TestApplyCountsChangesCounters(t *testing.T) {
	tests := []struct {
		name    string
		start   agonesv1.CounterStatus
		updates []CounterUpdate
		want    agonesv1.CounterStatus
	}{
		{
			name:    "a difference is added to the count",
			start:   agonesv1.CounterStatus{Count: 10, Capacity: 100},
			updates: []CounterUpdate{{Name: "players", CountDiff: 9}},
			want:    agonesv1.CounterStatus{Count: 19, Capacity: 100},
		},
		{
			name:    "a negative difference is taken off the count",
			start:   agonesv1.CounterStatus{Count: 10, Capacity: 100},
			updates: []CounterUpdate{{Name: "players", CountDiff: -1}},
			want:    agonesv1.CounterStatus{Count: 9, Capacity: 100},
		},
		{
			name:    "the count is set",
			start:   agonesv1.CounterStatus{Count: 10, Capacity: 100},
			updates: []CounterUpdate{{Name: "players", Count: new(int64(42))}},
			want:    agonesv1.CounterStatus{Count: 42, Capacity: 100},
		},
		{
			name:    "a smaller capacity takes the count down with it",
			start:   agonesv1.CounterStatus{Count: 10, Capacity: 100},
			updates: []CounterUpdate{{Name: "players", Capacity: new(int64(0))}},
			want:    agonesv1.CounterStatus{Count: 0, Capacity: 0},
		},
		{
			name:    "capacity comes first, then count, then the difference",
			start:   agonesv1.CounterStatus{Count: 10, Capacity: 100},
			updates: []CounterUpdate{{Name: "players", Capacity: new(int64(25)), Count: new(int64(0)), CountDiff: 25}},
			want:    agonesv1.CounterStatus{Count: 25, Capacity: 25},
		},
		{
			name:    "an increment stops at a capacity that shrank after the sidecar accepted it",
			start:   agonesv1.CounterStatus{Count: 10, Capacity: 12},
			updates: []CounterUpdate{{Name: "players", CountDiff: 5}},
			want:    agonesv1.CounterStatus{Count: 12, Capacity: 12},
		},
		{
			name:    "a decrement stops at zero when the count dropped after the sidecar accepted it",
			start:   agonesv1.CounterStatus{Count: 2, Capacity: 100},
			updates: []CounterUpdate{{Name: "players", CountDiff: -5}},
			want:    agonesv1.CounterStatus{Count: 0, Capacity: 100},
		},
		{
			name:    "a count past the capacity is cut to it",
			start:   agonesv1.CounterStatus{Count: 10, Capacity: 20},
			updates: []CounterUpdate{{Name: "players", Count: new(int64(42))}},
			want:    agonesv1.CounterStatus{Count: 20, Capacity: 20},
		},
		{
			name:  "each update builds on the one before",
			start: agonesv1.CounterStatus{Count: 10, Capacity: 100},
			updates: []CounterUpdate{
				{Name: "players", Capacity: new(int64(5))},
				{Name: "players", CountDiff: -3},
				{Name: "players", Capacity: new(int64(50))},
				{Name: "players", CountDiff: 10},
			},
			want: agonesv1.CounterStatus{Count: 12, Capacity: 50},
		},
		{
			name:    "an update to a Counter that is gone is skipped",
			start:   agonesv1.CounterStatus{Count: 10, Capacity: 100},
			updates: []CounterUpdate{{Name: "gone", CountDiff: 1}},
			want:    agonesv1.CounterStatus{Count: 10, Capacity: 100},
		},
		{
			name:    "a negative capacity is ignored",
			start:   agonesv1.CounterStatus{Count: 10, Capacity: 100},
			updates: []CounterUpdate{{Name: "players", Capacity: new(int64(-1)), CountDiff: 1}},
			want:    agonesv1.CounterStatus{Count: 11, Capacity: 100},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gs := &agonesv1.GameServer{}
			gs.Status.Counters = map[string]agonesv1.CounterStatus{"players": tc.start}

			ApplyCounts(gs, tc.updates, nil)

			assert.Equal(t, map[string]agonesv1.CounterStatus{"players": tc.want}, gs.Status.Counters)
		})
	}
}

func TestApplyCountsChangesLists(t *testing.T) {
	tests := []struct {
		name    string
		start   agonesv1.ListStatus
		updates []ListUpdate
		want    agonesv1.ListStatus
	}{
		{
			name:    "a value is added at the end",
			start:   agonesv1.ListStatus{Capacity: 10, Values: []string{"one", "two"}},
			updates: []ListUpdate{{Name: "rooms", Add: []string{"three"}}},
			want:    agonesv1.ListStatus{Capacity: 10, Values: []string{"one", "two", "three"}},
		},
		{
			name:    "a value the List already holds is not added again",
			start:   agonesv1.ListStatus{Capacity: 10, Values: []string{"one", "two"}},
			updates: []ListUpdate{{Name: "rooms", Add: []string{"two", "three", "three"}}},
			want:    agonesv1.ListStatus{Capacity: 10, Values: []string{"one", "two", "three"}},
		},
		{
			name:    "a value that no longer fits is cut off",
			start:   agonesv1.ListStatus{Capacity: 3, Values: []string{"one", "two", "three"}},
			updates: []ListUpdate{{Name: "rooms", Add: []string{"four"}}},
			want:    agonesv1.ListStatus{Capacity: 3, Values: []string{"one", "two", "three"}},
		},
		{
			name:    "a value is removed, and one the List does not hold is ignored",
			start:   agonesv1.ListStatus{Capacity: 10, Values: []string{"one", "two", "three"}},
			updates: []ListUpdate{{Name: "rooms", Remove: []string{"two", "nine"}}},
			want:    agonesv1.ListStatus{Capacity: 10, Values: []string{"one", "three"}},
		},
		{
			name:    "removing every value leaves an empty List",
			start:   agonesv1.ListStatus{Capacity: 10, Values: []string{"one", "two"}},
			updates: []ListUpdate{{Name: "rooms", Remove: []string{"two", "one"}}},
			want:    agonesv1.ListStatus{Capacity: 10, Values: []string{}},
		},
		{
			name:    "a smaller capacity cuts the values",
			start:   agonesv1.ListStatus{Capacity: 10, Values: []string{"one", "two", "three"}},
			updates: []ListUpdate{{Name: "rooms", Capacity: new(int64(1))}},
			want:    agonesv1.ListStatus{Capacity: 1, Values: []string{"one"}},
		},
		{
			name:    "a capacity of zero empties the List",
			start:   agonesv1.ListStatus{Capacity: 10, Values: []string{"one", "two", "three"}},
			updates: []ListUpdate{{Name: "rooms", Capacity: new(int64(0))}},
			want:    agonesv1.ListStatus{Capacity: 0, Values: []string{}},
		},
		{
			name:    "the values of a full List can be replaced, because cutting comes last",
			start:   agonesv1.ListStatus{Capacity: 2, Values: []string{"one", "two"}},
			updates: []ListUpdate{{Name: "rooms", Add: []string{"three", "four"}, Remove: []string{"one", "two"}}},
			want:    agonesv1.ListStatus{Capacity: 2, Values: []string{"three", "four"}},
		},
		{
			name:    "what is removed makes room before the values are cut",
			start:   agonesv1.ListStatus{Capacity: 10, Values: []string{"one", "two", "three", "four", "five"}},
			updates: []ListUpdate{{Name: "rooms", Capacity: new(int64(3)), Remove: []string{"one"}}},
			want:    agonesv1.ListStatus{Capacity: 3, Values: []string{"two", "three", "four"}},
		},
		{
			name:  "each update builds on the one before",
			start: agonesv1.ListStatus{Capacity: 2, Values: []string{"one", "two"}},
			updates: []ListUpdate{
				{Name: "rooms", Remove: []string{"one"}},
				{Name: "rooms", Add: []string{"three"}},
				{Name: "rooms", Add: []string{"one"}},
			},
			want: agonesv1.ListStatus{Capacity: 2, Values: []string{"two", "three"}},
		},
		{
			name:    "an update to a List that is gone is skipped",
			start:   agonesv1.ListStatus{Capacity: 10, Values: []string{"one"}},
			updates: []ListUpdate{{Name: "gone", Add: []string{"two"}}},
			want:    agonesv1.ListStatus{Capacity: 10, Values: []string{"one"}},
		},
		{
			name:    "a negative capacity is ignored",
			start:   agonesv1.ListStatus{Capacity: 10, Values: []string{"one"}},
			updates: []ListUpdate{{Name: "rooms", Capacity: new(int64(-1)), Add: []string{"two"}}},
			want:    agonesv1.ListStatus{Capacity: 10, Values: []string{"one", "two"}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gs := &agonesv1.GameServer{}
			gs.Status.Lists = map[string]agonesv1.ListStatus{"rooms": tc.start}

			ApplyCounts(gs, nil, tc.updates)

			assert.Equal(t, map[string]agonesv1.ListStatus{"rooms": tc.want}, gs.Status.Lists)
		})
	}
}

func TestApplyCountsCopesWithAGameServerThatHasNoCountersOrLists(t *testing.T) {
	gs := &agonesv1.GameServer{}

	ApplyCounts(gs, []CounterUpdate{{Name: "players", CountDiff: 1}}, []ListUpdate{{Name: "rooms", Add: []string{"one"}}})

	assert.Empty(t, gs.Status.Counters)
	assert.Empty(t, gs.Status.Lists)
}

// told is the content of one request from a sidecar.
type told struct {
	counters []CounterUpdate
	lists    []ListUpdate
}

// The sidecar answers the game server from its own prediction of what the
// control plane will store. If the two ever disagreed, a game server would
// read one value and the rest of the world another.
func TestTheSidecarPredictsWhatApplyCountsDoes(t *testing.T) {
	tests := []struct {
		name     string
		requests []told
	}{
		{
			name: "a Counter is resized, moved and set",
			requests: []told{
				{counters: []CounterUpdate{{Name: "players", Capacity: new(int64(5))}}},
				{counters: []CounterUpdate{{Name: "players", CountDiff: -3}}},
				{counters: []CounterUpdate{{Name: "players", Capacity: new(int64(50)), Count: new(int64(20)), CountDiff: 5}}},
			},
		},
		{
			name: "a count runs into both ends of its range",
			requests: []told{
				{counters: []CounterUpdate{{Name: "players", CountDiff: 95}}},
				{counters: []CounterUpdate{{Name: "players", CountDiff: 10}}},
				{counters: []CounterUpdate{{Name: "players", CountDiff: -500}}},
				{counters: []CounterUpdate{{Name: "players", Count: new(int64(1000))}}},
			},
		},
		{
			name: "several Counters in one request, one of them gone",
			requests: []told{
				{counters: []CounterUpdate{
					{Name: "players", CountDiff: 1},
					{Name: "gone", CountDiff: 1},
					{Name: "sessions", Count: new(int64(5))},
					{Name: "sessions", CountDiff: 1},
				}},
			},
		},
		{
			name: "values are added to a List until it is full, then one is swapped",
			requests: []told{
				{lists: []ListUpdate{{Name: "rooms", Add: []string{"c", "d", "e"}}}},
				{lists: []ListUpdate{{Name: "rooms", Remove: []string{"a"}}}},
				{lists: []ListUpdate{{Name: "rooms", Add: []string{"e"}}}},
			},
		},
		{
			name: "the values of a List are replaced while its capacity shrinks",
			requests: []told{
				{lists: []ListUpdate{{Name: "rooms", Capacity: new(int64(3)), Add: []string{"x", "y", "z"}, Remove: []string{"a", "b", "c"}}}},
			},
		},
		{
			name: "a List shrinks, loses a value and grows again",
			requests: []told{
				{lists: []ListUpdate{{Name: "rooms", Capacity: new(int64(2)), Remove: []string{"a"}}}},
				{lists: []ListUpdate{{Name: "rooms", Capacity: new(int64(10)), Add: []string{"q"}}}},
				{lists: []ListUpdate{{Name: "gone", Add: []string{"q"}}}},
			},
		},
		{
			name: "one update adds and removes the same value",
			requests: []told{
				{lists: []ListUpdate{{Name: "rooms", Add: []string{"x"}, Remove: []string{"x", "b"}}}},
			},
		},
		{
			name: "a List that holds a value twice gains another",
			requests: []told{
				{lists: []ListUpdate{{Name: "maps", Add: []string{"town", "town"}}}},
				{lists: []ListUpdate{{Name: "maps", Remove: []string{"dust"}}}},
			},
		},
		{
			name: "Counters and Lists change in the same request",
			requests: []told{
				{
					counters: []CounterUpdate{{Name: "players", CountDiff: 1}},
					lists:    []ListUpdate{{Name: "rooms", Add: []string{"d"}}},
				},
				{
					counters: []CounterUpdate{{Name: "players", CountDiff: -1}},
					lists:    []ListUpdate{{Name: "rooms", Remove: []string{"d"}}},
				},
			},
		},
		{
			name: "a broken sender asks for negative capacities",
			requests: []told{
				{counters: []CounterUpdate{{Name: "players", Capacity: new(int64(-1)), CountDiff: 1}}},
				{lists: []ListUpdate{{Name: "rooms", Capacity: new(int64(-1)), Add: []string{"z"}}}},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gs := &agonesv1.GameServer{}
			gs.Status.Counters = map[string]agonesv1.CounterStatus{
				"players":  {Count: 10, Capacity: 100},
				"sessions": {Count: 3, Capacity: 5},
			}
			gs.Status.Lists = map[string]agonesv1.ListStatus{
				"rooms": {Capacity: 4, Values: []string{"a", "b", "c"}},
				"maps":  {Capacity: 10, Values: []string{"dust", "dust", "aztec"}},
			}
			before := sdkView(gs)

			var pending []*sidecarv1.TellRequest
			for _, r := range tc.requests {
				ApplyCounts(gs, r.counters, r.lists)
				pending = append(pending, request(r))
			}

			stored, predicted := sdkView(gs), sdkserver.WithPending(before, pending)
			assert.True(t, proto.Equal(stored, predicted), "stored:    %v\npredicted: %v", stored.GetStatus(), predicted.GetStatus())
		})
	}
}

// request is r the way a sidecar sends it: the reverse of what the agent
// does before it calls ApplyCounts.
func request(r told) *sidecarv1.TellRequest {
	req := &sidecarv1.TellRequest{Event: sidecarv1.Event_EVENT_UPDATE_COUNTS}
	for _, u := range r.counters {
		req.Counters = append(req.Counters, &sidecarv1.CounterUpdate{Name: u.Name, Capacity: u.Capacity, Count: u.Count, CountDiff: u.CountDiff})
	}
	for _, u := range r.lists {
		req.Lists = append(req.Lists, &sidecarv1.ListUpdate{Name: u.Name, Capacity: u.Capacity, Add: u.Add, Remove: u.Remove})
	}
	return req
}

// sdkView returns the Counters and Lists of gs the way the SDK shows them.
func sdkView(gs *agonesv1.GameServer) *sdk.GameServer {
	status := &sdk.GameServer_Status{
		Counters: map[string]*sdk.GameServer_Status_CounterStatus{},
		Lists:    map[string]*sdk.GameServer_Status_ListStatus{},
	}
	for name, c := range gs.Status.Counters {
		status.Counters[name] = &sdk.GameServer_Status_CounterStatus{Count: c.Count, Capacity: c.Capacity}
	}
	for name, l := range gs.Status.Lists {
		status.Lists[name] = &sdk.GameServer_Status_ListStatus{Capacity: l.Capacity, Values: slices.Clone(l.Values)}
	}
	return &sdk.GameServer{Status: status}
}
