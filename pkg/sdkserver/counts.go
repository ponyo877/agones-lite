package sdkserver

import (
	"context"
	"fmt"
	"slices"

	"agones.dev/agones/pkg/sdk"
	sdkbeta "agones.dev/agones/pkg/sdk/beta"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"

	sidecarv1 "github.com/ponyo877/agones-lite/proto/sidecar/v1"
)

// maxListCapacity is the largest capacity a List may be given.
// Upstream: defaultMaxListItems in cmd/sdk-server/main.go.
const maxListCapacity = 1000

// beta is the beta SDK service: Counters and Lists.
//
// A request is judged against the view. If it is accepted, the change is
// made in the view at once and queued for the agent, so the answer and every
// later read show it. Upstream gets there by batching: it holds the changes
// of the last moments in memory and lays them over every read.
//
// A refusal is a plain error with upstream's wording. Clients therefore get
// codes.Unknown and not the codes the proto promises, as they do upstream.
type beta struct {
	sdkbeta.UnimplementedSDKServer
	server *Server
}

// GetCounter returns a Counter.
func (b beta) GetCounter(ctx context.Context, in *sdkbeta.GetCounterRequest) (*sdkbeta.Counter, error) {
	view, err := b.server.get(ctx)
	if err != nil {
		return nil, err
	}
	return counterOf(view, in.GetName())
}

// UpdateCounter sets a Counter's capacity, sets its count, moves its count,
// or any of these together, and returns the Counter as changed.
func (b beta) UpdateCounter(ctx context.Context, in *sdkbeta.UpdateCounterRequest) (*sdkbeta.Counter, error) {
	update := in.GetCounterUpdateRequest()
	changed, err := b.server.change(ctx, func(view *sdk.GameServer) (*sidecarv1.TellRequest, error) {
		return counterChange(view, update)
	})
	if err != nil {
		return nil, err
	}
	return counterOf(changed, update.GetName())
}

// GetList returns a List.
func (b beta) GetList(ctx context.Context, in *sdkbeta.GetListRequest) (*sdkbeta.List, error) {
	view, err := b.server.get(ctx)
	if err != nil {
		return nil, err
	}
	return listOf(view, in.GetName())
}

// UpdateList sets a List's capacity, replaces its values, or both: whatever
// the field mask names. Like upstream it answers with an empty List, not
// with the updated one the proto promises.
func (b beta) UpdateList(ctx context.Context, in *sdkbeta.UpdateListRequest) (*sdkbeta.List, error) {
	_, err := b.server.change(ctx, func(view *sdk.GameServer) (*sidecarv1.TellRequest, error) {
		return listChange(view, in)
	})
	if err != nil {
		return nil, err
	}
	return &sdkbeta.List{}, nil
}

// AddListValue appends a value to a List and returns the List as changed.
func (b beta) AddListValue(ctx context.Context, in *sdkbeta.AddListValueRequest) (*sdkbeta.List, error) {
	changed, err := b.server.change(ctx, func(view *sdk.GameServer) (*sidecarv1.TellRequest, error) {
		return listAddition(view, in.GetName(), in.GetValue())
	})
	if err != nil {
		return nil, err
	}
	return listOf(changed, in.GetName())
}

// RemoveListValue removes a value from a List and returns the List as changed.
func (b beta) RemoveListValue(ctx context.Context, in *sdkbeta.RemoveListValueRequest) (*sdkbeta.List, error) {
	changed, err := b.server.change(ctx, func(view *sdk.GameServer) (*sidecarv1.TellRequest, error) {
		return listRemoval(view, in.GetName(), in.GetValue())
	})
	if err != nil {
		return nil, err
	}
	return listOf(changed, in.GetName())
}

// counterChange judges an UpdateCounter request.
// Upstream: UpdateCounter in pkg/sdkserver/sdkserver.go.
func counterChange(view *sdk.GameServer, in *sdkbeta.CounterUpdateRequest) (*sidecarv1.TellRequest, error) {
	if in == nil {
		return nil, fmt.Errorf("invalid argument. CounterUpdateRequest: %v cannot be nil", in)
	}
	if in.GetCountDiff() == 0 && in.GetCount() == nil && in.GetCapacity() == nil {
		return nil, fmt.Errorf("invalid argument. Malformed CounterUpdateRequest: %v", in)
	}
	counter, err := counterIn(view, in.GetName())
	if err != nil {
		return nil, err
	}
	if err := checkCounter(counter, in); err != nil {
		return nil, err
	}
	return counterRequest(&sidecarv1.CounterUpdate{
		Name:      in.GetName(),
		Capacity:  optional(in.GetCapacity()),
		Count:     optional(in.GetCount()),
		CountDiff: in.GetCountDiff(),
	}), nil
}

// checkCounter refuses a request that would leave the Counter out of range.
// A request may set the capacity, set the count and move the count all at
// once; each step is checked against the ones before it.
func checkCounter(counter *sdk.GameServer_Status_CounterStatus, in *sdkbeta.CounterUpdateRequest) error {
	capacity, count := counter.GetCapacity(), counter.GetCount()
	if in.GetCapacity() != nil {
		capacity = in.GetCapacity().GetValue()
		if capacity < 0 {
			return fmt.Errorf("out of range. Capacity must be greater than or equal to 0. Found Capacity: %d", capacity)
		}
	}
	if in.GetCount() != nil {
		count = in.GetCount().GetValue()
		if err := checkCount(count, capacity); err != nil {
			return err
		}
	}
	if in.GetCountDiff() != 0 {
		return checkCount(count+in.GetCountDiff(), capacity)
	}
	return nil
}

// checkCount refuses a count outside of [0, capacity].
func checkCount(count, capacity int64) error {
	if count < 0 || count > capacity {
		return fmt.Errorf("out of range. Count must be within range [0,Capacity]. Found Count: %d, Capacity: %d", count, capacity)
	}
	return nil
}

// listChange judges an UpdateList request. The capacity in the request must
// be in range even when the mask leaves the capacity alone.
// Upstream: UpdateList in pkg/sdkserver/sdkserver.go.
func listChange(view *sdk.GameServer, in *sdkbeta.UpdateListRequest) (*sidecarv1.TellRequest, error) {
	list, mask := in.GetList(), in.GetUpdateMask()
	if list == nil || mask == nil {
		return nil, fmt.Errorf("invalid argument. List: %v and UpdateMask %v cannot be nil", list, mask)
	}
	if !mask.IsValid(list) {
		return nil, fmt.Errorf("invalid argument. Field Mask Path(s): %v are invalid for List. Use valid field name(s): %v", mask.GetPaths(), list.ProtoReflect().Descriptor().Fields())
	}
	if list.GetCapacity() < 0 || list.GetCapacity() > maxListCapacity {
		return nil, fmt.Errorf("out of range. Capacity must be within range [0,%d]. Found Capacity: %d", maxListCapacity, list.GetCapacity())
	}
	current, err := listIn(view, list.GetName())
	if err != nil {
		// Upstream words this differently here than in the other calls.
		return nil, fmt.Errorf("not found. %s List not found", list.GetName())
	}
	update := &sidecarv1.ListUpdate{Name: list.GetName()}
	if slices.Contains(mask.GetPaths(), "capacity") {
		update.Capacity = proto.Int64(list.GetCapacity())
	}
	if slices.Contains(mask.GetPaths(), "values") {
		// Values the List keeps stay where they are; new ones go to the end.
		update.Add = without(list.GetValues(), current.GetValues())
		update.Remove = without(current.GetValues(), list.GetValues())
	}
	return listRequest(update), nil
}

// listAddition judges an AddListValue request. Room is checked before the
// value is, so a full List says "out of range" even for a value it holds.
// Upstream: AddListValue in pkg/sdkserver/sdkserver.go.
func listAddition(view *sdk.GameServer, name, value string) (*sidecarv1.TellRequest, error) {
	list, err := listIn(view, name)
	if err != nil {
		return nil, err
	}
	if size := len(list.GetValues()); int(list.GetCapacity()) <= size {
		return nil, fmt.Errorf("out of range. No available capacity. Current Capacity: %d, List Size: %d", list.GetCapacity(), size)
	}
	if slices.Contains(list.GetValues(), value) {
		return nil, fmt.Errorf("already exists. Value: %s already in List: %s", value, name)
	}
	return listRequest(&sidecarv1.ListUpdate{Name: name, Add: []string{value}}), nil
}

// listRemoval judges a RemoveListValue request.
// Upstream: RemoveListValue in pkg/sdkserver/sdkserver.go.
func listRemoval(view *sdk.GameServer, name, value string) (*sidecarv1.TellRequest, error) {
	list, err := listIn(view, name)
	if err != nil {
		return nil, err
	}
	if !slices.Contains(list.GetValues(), value) {
		return nil, fmt.Errorf("not found: value %s not in list %s", value, name)
	}
	return listRequest(&sidecarv1.ListUpdate{Name: name, Remove: []string{value}}), nil
}

// counterRequest wraps a change to a Counter for the agent.
func counterRequest(update *sidecarv1.CounterUpdate) *sidecarv1.TellRequest {
	return &sidecarv1.TellRequest{Event: sidecarv1.Event_EVENT_UPDATE_COUNTS, Counters: []*sidecarv1.CounterUpdate{update}}
}

// listRequest wraps a change to a List for the agent.
func listRequest(update *sidecarv1.ListUpdate) *sidecarv1.TellRequest {
	return &sidecarv1.TellRequest{Event: sidecarv1.Event_EVENT_UPDATE_COUNTS, Lists: []*sidecarv1.ListUpdate{update}}
}

// optional turns a wrapped number of the SDK into an optional field of the
// sidecar protocol.
func optional(wrapped *wrapperspb.Int64Value) *int64 {
	if wrapped == nil {
		return nil
	}
	return proto.Int64(wrapped.GetValue())
}

// counterIn finds a Counter in the view.
func counterIn(view *sdk.GameServer, name string) (*sdk.GameServer_Status_CounterStatus, error) {
	counter, ok := view.GetStatus().GetCounters()[name]
	if !ok {
		return nil, fmt.Errorf("counter not found: %s", name)
	}
	return counter, nil
}

// listIn finds a List in the view.
func listIn(view *sdk.GameServer, name string) (*sdk.GameServer_Status_ListStatus, error) {
	list, ok := view.GetStatus().GetLists()[name]
	if !ok {
		return nil, fmt.Errorf("list not found: %s", name)
	}
	return list, nil
}

// counterOf returns a Counter in the shape the SDK hands out.
func counterOf(view *sdk.GameServer, name string) (*sdkbeta.Counter, error) {
	counter, err := counterIn(view, name)
	if err != nil {
		return nil, err
	}
	return &sdkbeta.Counter{Name: name, Count: counter.GetCount(), Capacity: counter.GetCapacity()}, nil
}

// listOf returns a List in the shape the SDK hands out.
func listOf(view *sdk.GameServer, name string) (*sdkbeta.List, error) {
	list, err := listIn(view, name)
	if err != nil {
		return nil, err
	}
	return &sdkbeta.List{Name: name, Capacity: list.GetCapacity(), Values: list.GetValues()}, nil
}
