package sdkserver

import (
	"context"
	"testing"
	"time"

	"agones.dev/agones/pkg/sdk"
	sdkbeta "agones.dev/agones/pkg/sdk/beta"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	sidecarv1 "github.com/ponyo877/agones-lite/proto/sidecar/v1"
)

// betaFor returns the beta service of a sidecar that has been sent gs.
// Nothing is delivered to the agent, so every change stays pending.
func betaFor(gs *sdk.GameServer) beta {
	s := New(&fakeAgent{})
	s.set(gs)
	return beta{server: s}
}

// Upstream: TestSDKServerUpdateCounter, plus the refusals it leaves out.
func TestUpdateCounterAcceptsAndRefusesLikeUpstream(t *testing.T) {
	const outOfRange = "out of range. Count must be within range [0,Capacity]. "
	tests := []struct {
		name     string
		requests []*sdkbeta.CounterUpdateRequest
		refusals []string // one for each request; empty when it is accepted
		want     counter  // the answer to an accepted request, and the Counter in the end
	}{
		{
			name:     "an increment",
			requests: []*sdkbeta.CounterUpdateRequest{{Name: "players", CountDiff: 9}},
			refusals: []string{""},
			want:     counter{19, 100},
		},
		{
			name:     "an increment past the capacity",
			requests: []*sdkbeta.CounterUpdateRequest{{Name: "players", CountDiff: 100}},
			refusals: []string{outOfRange + "Found Count: 110, Capacity: 100"},
			want:     counter{10, 100},
		},
		{
			name:     "a decrement",
			requests: []*sdkbeta.CounterUpdateRequest{{Name: "players", CountDiff: -1}},
			refusals: []string{""},
			want:     counter{9, 100},
		},
		{
			name:     "a decrement below zero",
			requests: []*sdkbeta.CounterUpdateRequest{{Name: "players", CountDiff: -11}},
			refusals: []string{outOfRange + "Found Count: -1, Capacity: 100"},
			want:     counter{10, 100},
		},
		{
			name:     "a capacity below the count takes the count down with it",
			requests: []*sdkbeta.CounterUpdateRequest{{Name: "players", Capacity: wrapperspb.Int64(0)}},
			refusals: []string{""},
			want:     counter{0, 0},
		},
		{
			name:     "a negative capacity",
			requests: []*sdkbeta.CounterUpdateRequest{{Name: "players", Capacity: wrapperspb.Int64(-1)}},
			refusals: []string{"out of range. Capacity must be greater than or equal to 0. Found Capacity: -1"},
			want:     counter{10, 100},
		},
		{
			name:     "a count",
			requests: []*sdkbeta.CounterUpdateRequest{{Name: "players", Count: wrapperspb.Int64(42)}},
			refusals: []string{""},
			want:     counter{42, 100},
		},
		{
			name:     "a count past the capacity",
			requests: []*sdkbeta.CounterUpdateRequest{{Name: "players", Count: wrapperspb.Int64(101)}},
			refusals: []string{outOfRange + "Found Count: 101, Capacity: 100"},
			want:     counter{10, 100},
		},
		{
			name: "an increment past a capacity that was set just before",
			requests: []*sdkbeta.CounterUpdateRequest{
				{Name: "players", Capacity: wrapperspb.Int64(0)},
				{Name: "players", CountDiff: 1},
			},
			refusals: []string{"", outOfRange + "Found Count: 1, Capacity: 0"},
			want:     counter{0, 0},
		},
		{
			name: "a decrement after the capacity was set to zero",
			requests: []*sdkbeta.CounterUpdateRequest{
				{Name: "players", Capacity: wrapperspb.Int64(0)},
				{Name: "players", CountDiff: -1},
			},
			refusals: []string{"", outOfRange + "Found Count: -1, Capacity: 0"},
			want:     counter{0, 0},
		},
		{
			name: "capacity, count and difference in one request",
			requests: []*sdkbeta.CounterUpdateRequest{
				{Name: "players", Capacity: wrapperspb.Int64(25), Count: wrapperspb.Int64(0), CountDiff: 25},
			},
			refusals: []string{""},
			want:     counter{25, 25},
		},
		{
			name: "a count is judged against the capacity in the same request",
			requests: []*sdkbeta.CounterUpdateRequest{
				{Name: "players", Capacity: wrapperspb.Int64(5), Count: wrapperspb.Int64(6)},
			},
			refusals: []string{outOfRange + "Found Count: 6, Capacity: 5"},
			want:     counter{10, 100},
		},
		{
			name: "a difference is judged against the count in the same request",
			requests: []*sdkbeta.CounterUpdateRequest{
				{Name: "players", Count: wrapperspb.Int64(99), CountDiff: 2},
			},
			refusals: []string{outOfRange + "Found Count: 101, Capacity: 100"},
			want:     counter{10, 100},
		},
		{
			name: "a difference is judged against the capacity in the same request, with the count not yet cut",
			requests: []*sdkbeta.CounterUpdateRequest{
				{Name: "players", Capacity: wrapperspb.Int64(5), CountDiff: -1},
			},
			refusals: []string{outOfRange + "Found Count: 9, Capacity: 5"},
			want:     counter{10, 100},
		},
		{
			name:     "a Counter that does not exist",
			requests: []*sdkbeta.CounterUpdateRequest{{Name: "nope", CountDiff: 1}},
			refusals: []string{"counter not found: nope"},
			want:     counter{10, 100},
		},
		{
			name:     "a request with nothing in it",
			requests: []*sdkbeta.CounterUpdateRequest{nil},
			refusals: []string{"invalid argument. CounterUpdateRequest: <nil> cannot be nil"},
			want:     counter{10, 100},
		},
	}
	ctx := context.Background()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := betaFor(withCounter("1", counter{10, 100}))
			accepted := 0
			for i, request := range tc.requests {
				got, err := b.UpdateCounter(ctx, &sdkbeta.UpdateCounterRequest{CounterUpdateRequest: request})
				if tc.refusals[i] != "" {
					assert.EqualError(t, err, tc.refusals[i])
					assert.Equal(t, codes.Unknown, status.Code(err), "upstream returns plain errors")
					continue
				}
				require.NoError(t, err)
				assert.Equal(t, tc.want, counter{got.GetCount(), got.GetCapacity()})
				accepted++
			}

			got, err := b.GetCounter(ctx, &sdkbeta.GetCounterRequest{Name: "players"})
			require.NoError(t, err)
			assert.Equal(t, "players", got.GetName())
			assert.Equal(t, tc.want, counter{got.GetCount(), got.GetCapacity()})
			assert.Len(t, b.server.pending, accepted, "only what was accepted goes to the agent")
		})
	}
}

func TestUpdateCounterRefusesARequestThatChangesNothing(t *testing.T) {
	b := betaFor(withCounter("1", counter{10, 100}))

	_, err := b.UpdateCounter(context.Background(), &sdkbeta.UpdateCounterRequest{
		CounterUpdateRequest: &sdkbeta.CounterUpdateRequest{Name: "players"},
	})
	assert.ErrorContains(t, err, "invalid argument. Malformed CounterUpdateRequest: ")
	assert.Empty(t, b.server.pending)
}

// Upstream: TestSDKServerAddListValue.
func TestAddListValueAcceptsAndRefusesLikeUpstream(t *testing.T) {
	const full = "out of range. No available capacity. Current Capacity: 10, List Size: 10"
	tests := []struct {
		name     string
		start    list
		values   []string
		refusals []string // one for each value; empty when it is accepted
		want     list
	}{
		{
			name:     "a value",
			start:    list{10, []string{"one", "two", "three", "four"}},
			values:   []string{"five"},
			refusals: []string{""},
			want:     list{10, []string{"one", "two", "three", "four", "five"}},
		},
		{
			name:   "values, some of which the List already holds",
			start:  list{10, []string{"one", "two", "three", "four"}},
			values: []string{"five", "one", "five", "zero"},
			refusals: []string{
				"",
				"already exists. Value: one already in List: rooms",
				"already exists. Value: five already in List: rooms",
				"",
			},
			want: list{10, []string{"one", "two", "three", "four", "five", "zero"}},
		},
		{
			name:     "one value more than there is room for",
			start:    list{10, []string{"one", "two", "three", "four"}},
			values:   []string{"five", "six", "seven", "eight", "nine", "ten", "eleven"},
			refusals: []string{"", "", "", "", "", "", full},
			want:     list{10, []string{"one", "two", "three", "four", "five", "six", "seven", "eight", "nine", "ten"}},
		},
		{
			name:     "a full List has no room, even for a value it holds",
			start:    list{10, []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "10"}},
			values:   []string{"1"},
			refusals: []string{full},
			want:     list{10, []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "10"}},
		},
	}
	ctx := context.Background()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := betaFor(withList("1", tc.start))
			for i, value := range tc.values {
				got, err := b.AddListValue(ctx, &sdkbeta.AddListValueRequest{Name: "rooms", Value: value})
				if tc.refusals[i] != "" {
					assert.EqualError(t, err, tc.refusals[i])
					assert.Equal(t, codes.Unknown, status.Code(err), "upstream returns plain errors")
					continue
				}
				require.NoError(t, err)
				assert.Equal(t, value, got.GetValues()[len(got.GetValues())-1], "the answer already has the value, at the end")
			}

			got, err := b.GetList(ctx, &sdkbeta.GetListRequest{Name: "rooms"})
			require.NoError(t, err)
			assert.Equal(t, "rooms", got.GetName())
			assert.Equal(t, tc.want, list{got.GetCapacity(), got.GetValues()})
		})
	}
}

// Upstream: TestSDKServerRemoveListValue.
func TestRemoveListValueAcceptsAndRefusesLikeUpstream(t *testing.T) {
	tests := []struct {
		name     string
		values   []string
		refusals []string // one for each value; empty when it is accepted
		want     list
	}{
		{
			name:     "a value",
			values:   []string{"two"},
			refusals: []string{""},
			want:     list{100, []string{"one", "three", "four"}},
		},
		{
			name:     "values, one of them twice",
			values:   []string{"two", "three", "two"},
			refusals: []string{"", "", "not found: value two not in list rooms"},
			want:     list{100, []string{"one", "four"}},
		},
		{
			name:     "every value",
			values:   []string{"three", "two", "four", "one"},
			refusals: []string{"", "", "", ""},
			want:     list{100, []string{}},
		},
	}
	ctx := context.Background()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := betaFor(withList("1", list{100, []string{"one", "two", "three", "four"}}))
			for i, value := range tc.values {
				got, err := b.RemoveListValue(ctx, &sdkbeta.RemoveListValueRequest{Name: "rooms", Value: value})
				if tc.refusals[i] != "" {
					assert.EqualError(t, err, tc.refusals[i])
					assert.Equal(t, codes.Unknown, status.Code(err), "upstream returns plain errors")
					continue
				}
				require.NoError(t, err)
				assert.NotContains(t, got.GetValues(), value, "the answer is already without the value")
			}

			got, err := b.GetList(ctx, &sdkbeta.GetListRequest{Name: "rooms"})
			require.NoError(t, err)
			assert.Equal(t, tc.want, list{got.GetCapacity(), got.GetValues()})
		})
	}
}

// Upstream collects the changes of half a second into a batch, and inside a
// batch three things go wrong. Here every change builds on the one before,
// which is what upstream gives when the same calls are further apart.
func TestABurstOfChangesGivesWhatTheSameChangesGiveFurtherApart(t *testing.T) {
	ctx := context.Background()

	t.Run("a value that was just added is not in the answer to its removal", func(t *testing.T) {
		b := betaFor(withList("1", list{10, []string{"one"}}))
		_, err := b.AddListValue(ctx, &sdkbeta.AddListValueRequest{Name: "rooms", Value: "two"})
		require.NoError(t, err)

		got, err := b.RemoveListValue(ctx, &sdkbeta.RemoveListValueRequest{Name: "rooms", Value: "two"})

		require.NoError(t, err)
		assert.Equal(t, []string{"one"}, got.GetValues(), "upstream answers with one and two")
	})

	t.Run("UpdateList keeps a value that was just added", func(t *testing.T) {
		b := betaFor(withList("1", list{10, []string{"one"}}))
		_, err := b.AddListValue(ctx, &sdkbeta.AddListValueRequest{Name: "rooms", Value: "two"})
		require.NoError(t, err)

		_, err = b.UpdateList(ctx, &sdkbeta.UpdateListRequest{
			List:       &sdkbeta.List{Name: "rooms", Capacity: 20},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"capacity"}},
		})

		require.NoError(t, err)
		got, err := b.GetList(ctx, &sdkbeta.GetListRequest{Name: "rooms"})
		require.NoError(t, err)
		assert.Equal(t, list{20, []string{"one", "two"}}, list{got.GetCapacity(), got.GetValues()}, "upstream drops two")
	})

	t.Run("a decrement is judged against the count that GetCounter shows", func(t *testing.T) {
		b := betaFor(withCounter("1", counter{10, 100}))
		_, err := b.UpdateCounter(ctx, &sdkbeta.UpdateCounterRequest{
			CounterUpdateRequest: &sdkbeta.CounterUpdateRequest{Name: "players", Capacity: wrapperspb.Int64(5)},
		})
		require.NoError(t, err)

		got, err := b.UpdateCounter(ctx, &sdkbeta.UpdateCounterRequest{
			CounterUpdateRequest: &sdkbeta.CounterUpdateRequest{Name: "players", CountDiff: -1},
		})

		require.NoError(t, err, "upstream refuses: it still reckons with a count of 10")
		assert.Equal(t, counter{4, 5}, counter{got.GetCount(), got.GetCapacity()})
	})
}

// Upstream: TestSDKServerUpdateList and TestSDKServerUpdateListMaxCapacity,
// plus what they leave out about the field mask.
func TestUpdateListAcceptsAndRefusesLikeUpstream(t *testing.T) {
	start := list{100, []string{"one", "two", "three", "four"}}
	six := []string{"one", "two", "three", "four", "five", "six"}
	mask := func(paths ...string) *fieldmaskpb.FieldMask { return &fieldmaskpb.FieldMask{Paths: paths} }
	tests := []struct {
		name    string
		request *sdkbeta.UpdateListRequest
		refusal string // part of the error; empty when the request is accepted
		want    list
	}{
		{
			name:    "the capacity can go up to the limit",
			request: &sdkbeta.UpdateListRequest{List: &sdkbeta.List{Name: "rooms", Capacity: 1000}, UpdateMask: mask("capacity")},
			want:    list{1000, start.values},
		},
		{
			name:    "a capacity of zero cuts every value",
			request: &sdkbeta.UpdateListRequest{List: &sdkbeta.List{Name: "rooms", Capacity: 0}, UpdateMask: mask("capacity")},
			want:    list{0, []string{}},
		},
		{
			name:    "a capacity past the limit",
			request: &sdkbeta.UpdateListRequest{List: &sdkbeta.List{Name: "rooms", Capacity: 1001}, UpdateMask: mask("capacity")},
			refusal: "out of range. Capacity must be within range [0,1000]. Found Capacity: 1001",
			want:    start,
		},
		{
			name:    "a negative capacity",
			request: &sdkbeta.UpdateListRequest{List: &sdkbeta.List{Name: "rooms", Capacity: -1}, UpdateMask: mask("capacity")},
			refusal: "out of range. Capacity must be within range [0,1000]. Found Capacity: -1",
			want:    start,
		},
		{
			name:    "values that fit",
			request: &sdkbeta.UpdateListRequest{List: &sdkbeta.List{Name: "rooms", Capacity: 100, Values: six}, UpdateMask: mask("capacity", "values")},
			want:    list{100, six},
		},
		{
			name:    "values that do not fit are cut",
			request: &sdkbeta.UpdateListRequest{List: &sdkbeta.List{Name: "rooms", Capacity: 4, Values: six}, UpdateMask: mask("capacity", "values")},
			want:    list{4, []string{"one", "two", "three", "four"}},
		},
		{
			name:    "the mask decides: only the values change here",
			request: &sdkbeta.UpdateListRequest{List: &sdkbeta.List{Name: "rooms", Capacity: 5, Values: []string{"five", "six"}}, UpdateMask: mask("values")},
			want:    list{100, []string{"five", "six"}},
		},
		{
			name:    "the mask decides: only the capacity changes here",
			request: &sdkbeta.UpdateListRequest{List: &sdkbeta.List{Name: "rooms", Capacity: 5, Values: []string{"five", "six"}}, UpdateMask: mask("capacity")},
			want:    list{5, start.values},
		},
		{
			name:    "values the List keeps stay where they are",
			request: &sdkbeta.UpdateListRequest{List: &sdkbeta.List{Name: "rooms", Values: []string{"four", "one", "five"}}, UpdateMask: mask("values")},
			want:    list{100, []string{"one", "four", "five"}},
		},
		{
			name:    "values in the mask but not in the request empty the List",
			request: &sdkbeta.UpdateListRequest{List: &sdkbeta.List{Name: "rooms"}, UpdateMask: mask("values")},
			want:    list{100, []string{}},
		},
		{
			name:    "a mask that names nothing changes nothing",
			request: &sdkbeta.UpdateListRequest{List: &sdkbeta.List{Name: "rooms", Capacity: 5}, UpdateMask: mask()},
			want:    start,
		},
		{
			name:    "the capacity must be in range even when the mask leaves it alone",
			request: &sdkbeta.UpdateListRequest{List: &sdkbeta.List{Name: "rooms", Capacity: 1001, Values: six}, UpdateMask: mask("values")},
			refusal: "out of range. Capacity must be within range [0,1000]. Found Capacity: 1001",
			want:    start,
		},
		{
			name:    "a mask that names something a List does not have",
			request: &sdkbeta.UpdateListRequest{List: &sdkbeta.List{Name: "rooms", Capacity: 5}, UpdateMask: mask("size")},
			refusal: "invalid argument. Field Mask Path(s): [size] are invalid for List. Use valid field name(s): ",
			want:    start,
		},
		{
			name:    "no mask",
			request: &sdkbeta.UpdateListRequest{List: &sdkbeta.List{Name: "rooms", Capacity: 5}},
			refusal: "and UpdateMask <nil> cannot be nil",
			want:    start,
		},
		{
			name:    "no List",
			request: &sdkbeta.UpdateListRequest{UpdateMask: mask("capacity")},
			refusal: "invalid argument. List: <nil> and UpdateMask ",
			want:    start,
		},
		{
			name:    "a List that does not exist, which is worded differently here",
			request: &sdkbeta.UpdateListRequest{List: &sdkbeta.List{Name: "nope", Capacity: 5}, UpdateMask: mask("capacity")},
			refusal: "not found. nope List not found",
			want:    start,
		},
	}
	ctx := context.Background()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := betaFor(withList("1", start))
			answer, err := b.UpdateList(ctx, tc.request)
			if tc.refusal != "" {
				assert.ErrorContains(t, err, tc.refusal)
				assert.Equal(t, codes.Unknown, status.Code(err), "upstream returns plain errors")
			} else {
				require.NoError(t, err)
				assert.True(t, proto.Equal(&sdkbeta.List{}, answer), "upstream answers with an empty List")
			}

			got, err := b.GetList(ctx, &sdkbeta.GetListRequest{Name: "rooms"})
			require.NoError(t, err)
			assert.Equal(t, tc.want, list{got.GetCapacity(), got.GetValues()})
		})
	}
}

func TestCountersAndListsThatDoNotExistAreRefused(t *testing.T) {
	ctx := context.Background()
	b := betaFor(withCounter("1", counter{10, 100}))

	_, err := b.GetCounter(ctx, &sdkbeta.GetCounterRequest{Name: "nope"})
	assert.EqualError(t, err, "counter not found: nope")
	_, err = b.GetList(ctx, &sdkbeta.GetListRequest{Name: "nope"})
	assert.EqualError(t, err, "list not found: nope")
	_, err = b.AddListValue(ctx, &sdkbeta.AddListValueRequest{Name: "nope", Value: "one"})
	assert.EqualError(t, err, "list not found: nope")
	_, err = b.RemoveListValue(ctx, &sdkbeta.RemoveListValueRequest{Name: "nope", Value: "one"})
	assert.EqualError(t, err, "list not found: nope")
	assert.Equal(t, codes.Unknown, status.Code(err), "upstream returns plain errors")
}

func TestCountersAndListsWaitForTheFirstGameServer(t *testing.T) {
	b := beta{server: New(&fakeAgent{})}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := b.GetCounter(ctx, &sdkbeta.GetCounterRequest{Name: "players"})
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	_, err = b.AddListValue(ctx, &sdkbeta.AddListValueRequest{Name: "rooms", Value: "one"})
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestTheAgentIsToldEveryChangeAsItWasMade(t *testing.T) {
	gs := withCounter("1", counter{10, 100})
	gs.Status.Lists = withList("1", list{100, []string{"one", "two", "three"}}).GetStatus().GetLists()
	agent := &fakeAgent{stored: gs}
	s := New(agent)
	s.set(gs)
	b := beta{server: s}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.deliver(ctx)

	_, err := b.UpdateCounter(ctx, &sdkbeta.UpdateCounterRequest{CounterUpdateRequest: &sdkbeta.CounterUpdateRequest{
		Name: "players", Capacity: wrapperspb.Int64(25), Count: wrapperspb.Int64(0), CountDiff: 25,
	}})
	require.NoError(t, err)
	_, err = b.AddListValue(ctx, &sdkbeta.AddListValueRequest{Name: "rooms", Value: "four"})
	require.NoError(t, err)
	_, err = b.RemoveListValue(ctx, &sdkbeta.RemoveListValueRequest{Name: "rooms", Value: "one"})
	require.NoError(t, err)
	_, err = b.UpdateList(ctx, &sdkbeta.UpdateListRequest{
		List:       &sdkbeta.List{Name: "rooms", Capacity: 50, Values: []string{"five", "three"}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"capacity", "values"}},
	})
	require.NoError(t, err)

	want := []*sidecarv1.TellRequest{
		counterRequest(&sidecarv1.CounterUpdate{Name: "players", Capacity: proto.Int64(25), Count: proto.Int64(0), CountDiff: 25}),
		listRequest(&sidecarv1.ListUpdate{Name: "rooms", Add: []string{"four"}}),
		listRequest(&sidecarv1.ListUpdate{Name: "rooms", Remove: []string{"one"}}),
		listRequest(&sidecarv1.ListUpdate{Name: "rooms", Capacity: proto.Int64(50), Add: []string{"five"}, Remove: []string{"two", "four"}}),
	}
	require.Eventually(t, func() bool { return len(agent.events()) == len(want) }, 2*time.Second, 10*time.Millisecond)
	names := map[string]bool{}
	for i := range want {
		names[agent.told[i].GetChangeId()] = true
		want[i].ChangeId = agent.told[i].GetChangeId()
		assert.True(t, proto.Equal(want[i], agent.told[i]), "request %d: %v", i, agent.told[i])
	}
	assert.Len(t, names, len(want), "every change has a name of its own")
	assert.NotContains(t, names, "")
}

func TestAChangeWhoseAnswerWasLostIsSentAgainUnderTheSameName(t *testing.T) {
	gs := withCounter("1", counter{10, 100})
	agent := &fakeAgent{stored: gs, losses: 1}
	s := New(agent)
	s.set(gs)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.deliver(ctx)

	_, err := beta{server: s}.UpdateCounter(ctx, &sdkbeta.UpdateCounterRequest{CounterUpdateRequest: &sdkbeta.CounterUpdateRequest{
		Name: "players", CountDiff: 1,
	}})
	require.NoError(t, err)

	require.Eventually(t, func() bool { return len(agent.events()) == 2 }, 5*time.Second, 10*time.Millisecond)
	assert.Equal(t, agent.told[0].GetChangeId(), agent.told[1].GetChangeId())
	// The agent made it once, and that is what the game server sees.
	counter, err := beta{server: s}.GetCounter(ctx, &sdkbeta.GetCounterRequest{Name: "players"})
	require.NoError(t, err)
	assert.Equal(t, int64(11), counter.GetCount())
}
