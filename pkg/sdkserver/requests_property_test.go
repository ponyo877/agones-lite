package sdkserver

import (
	"context"
	"math"
	"slices"
	"testing"

	"agones.dev/agones/pkg/sdk"
	sdkbeta "agones.dev/agones/pkg/sdk/beta"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/wrapperspb"
	"pgregory.net/rapid"
)

// A game server is somebody else's program. Whatever it asks through the
// SDK, in whatever order, the sidecar must stay up, and the Counters and
// Lists it shows must stay in order: a count from zero to its capacity, a
// List no longer than its capacity and without a value twice. The matchmaker
// reads these numbers to decide where players go.
//
// Nothing is delivered to the agent here, so every accepted change stays
// pending and what the sidecar shows is its own prediction.
func TestWhateverAGameServerAsksTheSidecarShowsCountersAndListsInOrder(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		b := betaFor(&sdk.GameServer{
			ObjectMeta: &sdk.GameServer_ObjectMeta{ResourceVersion: "1"},
			Status: &sdk.GameServer_Status{
				Counters: map[string]*sdk.GameServer_Status_CounterStatus{"players": {Count: 2, Capacity: 10}},
				Lists:    map[string]*sdk.GameServer_Status_ListStatus{"rooms": {Capacity: 3, Values: []string{"a", "b"}}},
			},
		})
		ctx := context.Background()

		t.Repeat(map[string]func(*rapid.T){
			"UpdateCounter": func(t *rapid.T) {
				_, _ = b.UpdateCounter(ctx, &sdkbeta.UpdateCounterRequest{CounterUpdateRequest: aCounterUpdate(t)})
			},
			"UpdateList": func(t *rapid.T) {
				_, _ = b.UpdateList(ctx, aListUpdate(t))
			},
			"AddListValue": func(t *rapid.T) {
				_, _ = b.AddListValue(ctx, &sdkbeta.AddListValueRequest{Name: aListName(t), Value: aValue(t)})
			},
			"RemoveListValue": func(t *rapid.T) {
				_, _ = b.RemoveListValue(ctx, &sdkbeta.RemoveListValueRequest{Name: aListName(t), Value: aValue(t)})
			},
			"a request with nothing in it": func(*rapid.T) {
				_, _ = b.UpdateCounter(ctx, &sdkbeta.UpdateCounterRequest{})
				_, _ = b.UpdateList(ctx, &sdkbeta.UpdateListRequest{})
				_, _ = b.GetCounter(ctx, &sdkbeta.GetCounterRequest{})
				_, _ = b.GetList(ctx, &sdkbeta.GetListRequest{})
			},
			"": func(t *rapid.T) {
				players, err := b.GetCounter(ctx, &sdkbeta.GetCounterRequest{Name: "players"})
				require.NoError(t, err)
				require.True(t, 0 <= players.GetCount() && players.GetCount() <= players.GetCapacity(),
					"players: count %d, capacity %d", players.GetCount(), players.GetCapacity())

				rooms, err := b.GetList(ctx, &sdkbeta.GetListRequest{Name: "rooms"})
				require.NoError(t, err)
				require.True(t, 0 <= rooms.GetCapacity() && rooms.GetCapacity() <= 1000, "rooms: capacity %d", rooms.GetCapacity())
				require.LessOrEqual(t, int64(len(rooms.GetValues())), rooms.GetCapacity(), "rooms: %v", rooms.GetValues())
				unique := slices.Compact(slices.Sorted(slices.Values(rooms.GetValues())))
				require.Len(t, unique, len(rooms.GetValues()), "rooms has a value twice: %v", rooms.GetValues())
			},
		})
	})
}

// aNumber draws a number that is small, at an edge, or absurd.
func aNumber(t *rapid.T, label string) int64 {
	return rapid.OneOf(
		rapid.Int64Range(-2, 12),
		rapid.SampledFrom([]int64{math.MinInt64, math.MinInt64 + 1, -1001, 1000, 1001, math.MaxInt64 - 1, math.MaxInt64}),
	).Draw(t, label)
}

// maybe draws a wrapped number, or none.
func maybe(t *rapid.T, label string) *wrapperspb.Int64Value {
	if rapid.Bool().Draw(t, label+" is set") {
		return wrapperspb.Int64(aNumber(t, label))
	}
	return nil
}

func aCounterUpdate(t *rapid.T) *sdkbeta.CounterUpdateRequest {
	return &sdkbeta.CounterUpdateRequest{
		Name:      rapid.SampledFrom([]string{"players", "players", "nobody", ""}).Draw(t, "counter"),
		Count:     maybe(t, "count"),
		Capacity:  maybe(t, "capacity"),
		CountDiff: aNumber(t, "diff"),
	}
}

func aListName(t *rapid.T) string {
	return rapid.SampledFrom([]string{"rooms", "rooms", "attic", ""}).Draw(t, "list")
}

func aValue(t *rapid.T) string {
	return rapid.SampledFrom([]string{"a", "b", "c", "d", "e", ""}).Draw(t, "value")
}

func aListUpdate(t *rapid.T) *sdkbeta.UpdateListRequest {
	req := &sdkbeta.UpdateListRequest{}
	if rapid.Bool().Draw(t, "names a list") {
		req.List = &sdkbeta.List{
			Name:     aListName(t),
			Capacity: aNumber(t, "capacity"),
			Values:   rapid.SliceOfN(rapid.Custom(aValue), 0, 5).Draw(t, "values"),
		}
	}
	if rapid.Bool().Draw(t, "has a mask") {
		paths := rapid.SliceOfN(rapid.SampledFrom([]string{"capacity", "values", "name", "nonsense", ""}), 0, 3).Draw(t, "paths")
		req.UpdateMask = &fieldmaskpb.FieldMask{Paths: paths}
	}
	return req
}
