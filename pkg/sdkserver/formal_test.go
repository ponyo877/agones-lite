package sdkserver

import (
	"testing"

	"agones.dev/agones/pkg/sdk"
	sdkbeta "agones.dev/agones/pkg/sdk/beta"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/ponyo877/scrim/formal/vectors"
)

// acceptanceVector is one answer of `accepted` in formal/Formal/Counts.lean.
type acceptanceVector struct {
	Count       int64  `json:"count"`
	Capacity    int64  `json:"capacity"`
	SetCapacity *int64 `json:"setCapacity"`
	SetCount    *int64 `json:"setCount"`
	Diff        int64  `json:"diff"`
	Accepted    bool   `json:"accepted"`
}

func (v acceptanceVector) request() *sdkbeta.CounterUpdateRequest {
	in := &sdkbeta.CounterUpdateRequest{Name: "rooms", CountDiff: v.Diff}
	if v.SetCapacity != nil {
		in.Capacity = wrapperspb.Int64(*v.SetCapacity)
	}
	if v.SetCount != nil {
		in.Count = wrapperspb.Int64(*v.SetCount)
	}
	return in
}

// The theorems in formal/Formal/Counts.lean say that what the sidecar
// accepts is stored without being cut. They are about a model; this test
// ties the model's `accepted` to checkCounter.
func TestCheckCounterAgreesWithTheLeanModel(t *testing.T) {
	all := vectors.Read[acceptanceVector](t, "counters.jsonl")
	require.NotEmpty(t, all)
	for i, v := range all {
		counter := &sdk.GameServer_Status_CounterStatus{Count: v.Count, Capacity: v.Capacity}

		err := checkCounter(counter, v.request())

		require.Equal(t, v.Accepted, err == nil, "vector %d: %+v: %v", i, v, err)
	}
}
