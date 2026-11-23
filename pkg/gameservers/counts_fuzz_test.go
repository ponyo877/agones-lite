package gameservers

import (
	"math"
	"testing"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"pgregory.net/rapid"

	"github.com/ponyo877/agones-lite/pkg/sdkserver"
	sidecarv1 "github.com/ponyo877/agones-lite/proto/sidecar/v1"
)

// The sidecar answers a game server from its own prediction of what the
// control plane will store, made by other code on other types. The cases in
// counts_test.go show that the two agree where somebody thought to look.
// Here rapid looks everywhere else, and `go test -fuzz` can keep looking
// for as long as it is given.

// aCount draws a number for a count, a capacity or a difference: small,
// below zero, or at either end of what an int64 holds.
func aCount() *rapid.Generator[int64] {
	return rapid.OneOf(rapid.Int64Range(-2, 6), rapid.SampledFrom([]int64{math.MinInt64, math.MaxInt64}))
}

// someValues draws a few values of a List from a handful, so that the same
// value comes up again and again: twice in one update, in what is added and
// in what is removed, and in a List that holds it already.
func someValues() *rapid.Generator[[]string] {
	return rapid.SliceOfN(rapid.SampledFrom([]string{"a", "b", "c", "d", "e"}), 0, 4)
}

// aGameServerWithCounts draws the Counters and Lists a GameServer may be
// stored with, the ones no update should have left behind included: a count
// beyond its capacity, a List that is over-full or holds a value twice.
func aGameServerWithCounts(t *rapid.T) *agonesv1.GameServer {
	gs := &agonesv1.GameServer{}
	gs.Status.Counters = rapid.MapOfN(rapid.SampledFrom([]string{"players", "sessions"}), rapid.Custom(func(t *rapid.T) agonesv1.CounterStatus {
		return agonesv1.CounterStatus{Count: aCount().Draw(t, "count"), Capacity: aCount().Draw(t, "capacity")}
	}), 0, 2).Draw(t, "counters")
	gs.Status.Lists = rapid.MapOfN(rapid.SampledFrom([]string{"rooms", "maps"}), rapid.Custom(func(t *rapid.T) agonesv1.ListStatus {
		return agonesv1.ListStatus{Capacity: aCount().Draw(t, "capacity"), Values: someValues().Draw(t, "values")}
	}), 0, 2).Draw(t, "lists")
	return gs
}

// aRequest draws what one request from a sidecar asks for. Nothing in it
// has to make sense: a sidecar refuses a negative capacity or the removal
// of a value that is not there, but the control plane must cope with a
// sender that did not.
func aRequest(t *rapid.T) told {
	return told{
		counters: rapid.SliceOfN(rapid.Custom(func(t *rapid.T) CounterUpdate {
			return CounterUpdate{
				Name:      rapid.SampledFrom([]string{"players", "sessions", "gone"}).Draw(t, "counter"),
				Capacity:  rapid.Ptr(aCount(), true).Draw(t, "capacity"),
				Count:     rapid.Ptr(aCount(), true).Draw(t, "count"),
				CountDiff: aCount().Draw(t, "countDiff"),
			}
		}), 0, 3).Draw(t, "counterUpdates"),
		lists: rapid.SliceOfN(rapid.Custom(func(t *rapid.T) ListUpdate {
			return ListUpdate{
				Name:     rapid.SampledFrom([]string{"rooms", "maps", "gone"}).Draw(t, "list"),
				Capacity: rapid.Ptr(aCount(), true).Draw(t, "capacity"),
				Add:      someValues().Draw(t, "add"),
				Remove:   someValues().Draw(t, "remove"),
			}
		}), 0, 3).Draw(t, "listUpdates"),
	}
}

// theSidecarPredictsWhatIsStored says: whatever a GameServer holds and
// whatever a row of requests asks for, the Counters and Lists the control
// plane ends up with are the ones the sidecar told the game server it
// would. If they ever differed, a game server would read one value and the
// rest of the world another, until the next change happened to bring them
// together.
func theSidecarPredictsWhatIsStored(t *rapid.T) {
	gs := aGameServerWithCounts(t)
	before := sdkView(gs)
	requests := rapid.SliceOfN(rapid.Custom(aRequest), 0, 5).Draw(t, "requests")

	var pending []*sidecarv1.TellRequest
	for _, r := range requests {
		ApplyCounts(gs, r.counters, r.lists)
		pending = append(pending, request(r))
	}

	stored, predicted := sdkView(gs), sdkserver.WithPending(before, pending)
	require.True(t, proto.Equal(stored, predicted), "stored:    %v\npredicted: %v", stored.GetStatus(), predicted.GetStatus())
}

func TestTheSidecarPredictsWhatApplyCountsDoesWhateverIsAsked(t *testing.T) {
	rapid.Check(t, theSidecarPredictsWhatIsStored)
}

// The same property for a long campaign:
//
//	go test ./pkg/gameservers -run '^$' -fuzz FuzzTheSidecarPredictsWhatApplyCountsDoes
func FuzzTheSidecarPredictsWhatApplyCountsDoes(f *testing.F) {
	f.Fuzz(rapid.MakeFuzz(theSidecarPredictsWhatIsStored))
}
