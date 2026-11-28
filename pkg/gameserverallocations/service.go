package gameserverallocations

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"

	"agones.dev/agones/pkg/allocation/converters"
	pb "agones.dev/agones/pkg/allocation/go"
	"agones.dev/agones/pkg/apis"
	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	allocationv1 "agones.dev/agones/pkg/apis/allocation/v1"
	gwruntime "github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"

	scrimv1 "github.com/ponyo877/scrim/pkg/apis/scrim/v1"
	"github.com/ponyo877/scrim/pkg/events"
	"github.com/ponyo877/scrim/pkg/gameservers"
	"github.com/ponyo877/scrim/pkg/store"
)

// attempts is how often an allocation is tried again after losing a race
// for its GameServer. Upstream: allocationRetry in allocator.go.
const attempts = 5

// errConflict is returned when every attempt lost its race. Upstream
// returns an error here too.
var errConflict = errors.New("could not update the selected GameServer")

// errBehind is returned while the store's file is too far behind for a new
// promise: see Service.Patience.
var errBehind = errors.New("the control plane cannot save what it decides right now, so it allocates nothing; try again shortly")

// patience is Service.Patience unless set otherwise. While the store keeps
// up, its file is behind by a few milliseconds.
const patience = time.Second

// KeyAnnotation is how a matchmaker names a request, so that it can ask
// again without being given a second GameServer:
//
//	metadata:
//	  annotations:
//	    scrim/allocation-key: <something of its own, new for each match>
//
// A matchmaker that got no answer cannot know whether a GameServer was
// taken for it. Asking again is all it can do, and without a name for the
// request, each asking takes another one. With the key, the GameServer
// that was taken is the answer for as long as its session lasts.
//
// It is an annotation, and the request has a place for those, so nothing
// is added to the API: the allocation puts it on the GameServer like any
// other, which is also where it is found again. A matchmaker that sets it
// can talk to Agones unchanged, where it is an annotation and no more.
const KeyAnnotation = "scrim/allocation-key"

// byKey sorts GameServers by the key of the request they were given to.
var byKey = store.By[*agonesv1.GameServer, string]{
	Name: "allocation-key",
	Of:   func(gs *agonesv1.GameServer) string { return gs.Annotations[KeyAnnotation] },
}

// Service is the allocation API, in the shape of upstream's allocator
// service: the same gRPC service and the same REST path, built from the
// same generated code. It is the thin half of this package.
//
// Requests choose their GameServer one at a time, which takes no waiting:
// the store has the choice at once. They wait for the store's file side by
// side, and the file takes them in one write (store.Run). Upstream gets
// the same effect by batching requests and updating a hundred GameServers
// in parallel.
//
// A request is given up at two points, both before a GameServer is taken
// for it. One is when its caller has stopped waiting: the answer would
// reach nobody, and the GameServer would be gone from the Ready ones all
// the same. The other is when the file is behind by more than Patience: a
// disk that stalls keeps every answer waiting, matchmakers time out and
// ask again, and each asking would take another GameServer. Refusing at
// once keeps what a stall can waste to what was asked for within Patience,
// and tells a matchmaker in milliseconds what it would otherwise learn
// from its own timeout. This is the delay budget of Protego (Cho et al.,
// NSDI 2023), applied to the one thing requests queue for here; that
// retries are what keeps an overload going after its cause has gone is
// from Bronson et al., "Metastable Failures in Distributed Systems"
// (HotOS 2021).
type Service struct {
	pb.UnimplementedAllocationServiceServer
	servers store.Typed[*agonesv1.GameServer]
	nodes   store.Typed[*scrimv1.Node]
	events  *events.Recorder
	now     func() time.Time
	// Pick chooses among n GameServers that are equally good, by giving a
	// number below n. It is random unless a test says otherwise.
	Pick func(n int) int
	// Observe, when set, is told how each request ended and how long it
	// took. It is how the metrics learn of allocations.
	Observe func(fleet, status, strategy string, took time.Duration)
	// Patience is how long the store's file may be behind before requests
	// are refused.
	Patience time.Duration

	turn chan struct{} // one allocation at a time: whoever has put its token in
}

// NewService returns a Service working on s.
func NewService(s *store.Store, recorder *events.Recorder) *Service {
	return &Service{
		servers:  store.GameServersOf(s),
		nodes:    store.NodesOf(s),
		events:   recorder,
		now:      s.Now,
		Pick:     rand.IntN,
		Patience: patience,
		turn:     make(chan struct{}, 1),
	}
}

// Allocate implements the allocation call. The request is prepared in
// upstream's order: defaults, validation, then the conversion of the
// deprecated selector fields.
func (s *Service) Allocate(ctx context.Context, in *pb.AllocationRequest) (*pb.AllocationResponse, error) {
	gsa := converters.ConvertAllocationRequestToGSA(in)
	gsa.ApplyDefaults()
	if errs := gsa.Validate(); len(errs) > 0 {
		return nil, status.Error(codes.InvalidArgument, errs.ToAggregate().Error())
	}
	gsa.Converter()
	if gsa.Spec.MultiClusterSetting.Enabled {
		return nil, status.Error(codes.Unimplemented, "scrim does not allocate across clusters")
	}
	started := s.now()
	result, err := s.allocate(ctx, gsa)
	if err == nil {
		// Players are sent on this answer, so it waits for the file. The
		// next request does not: see allocate.
		err = s.servers.Sync(ctx)
	}
	s.observe(gsa, result, s.now().Sub(started))
	if err != nil {
		return nil, refusal(err)
	}
	return converters.ConvertGSAToAllocationResponse(result, codes.ResourceExhausted)
}

// refusal says in gRPC's terms why a request got no answer. Unavailable is
// the one a client is expected to try again after a pause.
func refusal(err error) error {
	switch {
	case errors.Is(err, errBehind):
		return status.Error(codes.Unavailable, err.Error())
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return status.FromContextError(err).Err()
	}
	return status.Error(codes.Aborted, err.Error())
}

// observe reports the outcome of a request. A failed one has no result.
func (s *Service) observe(gsa, result *allocationv1.GameServerAllocation, took time.Duration) {
	if s.Observe == nil {
		return
	}
	outcome, fleet := "Error", "none"
	if result != nil {
		outcome = string(result.Status.State)
		if name := result.Status.Metadata; name != nil && name.Labels[agonesv1.FleetNameLabel] != "" {
			fleet = name.Labels[agonesv1.FleetNameLabel]
		}
	}
	s.Observe(fleet, outcome, string(gsa.Spec.Scheduling), took)
}

// allocate finds a GameServer and writes it back as Allocated. Losing the
// write to a concurrent change, such as the game server shutting down, means
// starting over with a fresh look.
func (s *Service) allocate(ctx context.Context, gsa *allocationv1.GameServerAllocation) (*allocationv1.GameServerAllocation, error) {
	if err := s.takeTurn(ctx); err != nil {
		return nil, err
	}
	defer func() { <-s.turn }()
	if gs, again := s.givenBefore(gsa); again {
		return Allocated(gsa, gs), nil
	}
	for range attempts {
		gs, found := s.find(gsa)
		if !found {
			return Unallocated(gsa), nil
		}
		allocated, problems := Apply(gsa, gs, s.now())
		_, err := s.servers.Update(allocated)
		if err == nil {
			s.record(allocated, problems)
			return Allocated(gsa, allocated), nil
		}
		if !errors.Is(err, store.ErrConflict) {
			return nil, err
		}
	}
	return nil, errConflict
}

// givenBefore returns the GameServer that a request with the same key was
// given, if its session is still on: see KeyAnnotation.
func (s *Service) givenBefore(gsa *allocationv1.GameServerAllocation) (*agonesv1.GameServer, bool) {
	key := gsa.Spec.MetaPatch.Annotations[KeyAnnotation]
	if key == "" {
		return nil, false
	}
	for _, gs := range store.Group(s.servers, byKey, key) {
		if candidate(gsa, gs) && gs.Status.State == agonesv1.GameServerStateAllocated {
			return gs, true
		}
	}
	return nil, false
}

// takeTurn waits until no other request is choosing, and refuses a request
// that should not get a GameServer any more: see Service.
func (s *Service) takeTurn(ctx context.Context) error {
	select {
	case s.turn <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	err := ctx.Err() // it may have had the turn and the end at once
	if lag := s.servers.Lag(); err == nil && lag > s.Patience {
		err = fmt.Errorf("%w (its file is %s behind)", errBehind, lag.Round(time.Millisecond))
	}
	if err != nil {
		<-s.turn
	}
	return err
}

// find chooses the GameServer for a request among those on the nodes
// players can be sent to: connected and not cordoned.
//
// Packed is the default, and needs no look at every GameServer. Neither
// does Distributed, most of the time: a request that asks for no order
// among the GameServers is given one by chance. What is left gets the look.
func (s *Service) find(gsa *allocationv1.GameServerAllocation) (*agonesv1.GameServer, bool) {
	counts := s.open()
	if gsa.Spec.Scheduling == apis.Packed {
		return FindPacked(gsa, counts, func(node string, state agonesv1.GameServerState) []*agonesv1.GameServer {
			return gameservers.OnNodeIn(s.servers, node, state)
		})
	}
	one := func(node string, state agonesv1.GameServerState, place int) (*agonesv1.GameServer, bool) {
		return gameservers.OneOnNodeIn(s.servers, node, state, place)
	}
	if gs, found := FindSpread(gsa, counts, one, s.Pick); found {
		return gs, true
	}
	return Find(gsa, s.servers.View(gsa.Namespace), counts, s.Pick)
}

// open returns how full each node is that players can be sent to. A node
// that is cordoned, cut off or gone is not in it, and neither is one
// without a Ready or Allocated GameServer. The development GameServers,
// whose games run on developers' machines, are on no node, and are
// counted under the empty name.
func (s *Service) open() map[string]gameservers.NodeCount {
	all := gameservers.CountOnNodes(s.servers)
	counts := map[string]gameservers.NodeCount{}
	for _, node := range s.nodes.View("") {
		if count, counted := all[node.Name]; counted && node.Schedulable() {
			counts[node.Name] = count
		}
	}
	if count, counted := gameservers.CountWithoutNode(s.servers); counted {
		counts[""] = count
	}
	return counts
}

func (s *Service) record(gs *agonesv1.GameServer, problems []error) {
	s.events.Record(gameservers.Kind, gs.Namespace, gs.Name, events.Normal, "Allocated", "by the allocation API")
	for _, problem := range problems {
		s.events.Record(gameservers.Kind, gs.Namespace, gs.Name, events.Warning, "AllocationAction", "%v", problem)
	}
}

// Handler serves the gRPC service and its REST form on one port, as
// upstream's allocator does when both are given the same port. It needs a
// TLS listener: gRPC over net/http requires HTTP/2.
func (s *Service) Handler(ctx context.Context) (http.Handler, error) {
	grpcServer := grpc.NewServer()
	pb.RegisterAllocationServiceServer(grpcServer, s)
	rest := gwruntime.NewServeMux(
		gwruntime.WithMarshalerOption(gwruntime.MIMEWildcard, &gwruntime.HTTPBodyMarshaler{
			Marshaler: &gwruntime.JSONPb{
				MarshalOptions:   protojson.MarshalOptions{UseProtoNames: true, EmitUnpopulated: true},
				UnmarshalOptions: protojson.UnmarshalOptions{DiscardUnknown: true},
			},
		}),
	)
	if err := pb.RegisterAllocationServiceHandlerServer(ctx, rest, s); err != nil {
		return nil, err
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor == 2 && strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
			grpcServer.ServeHTTP(w, r)
			return
		}
		rest.ServeHTTP(w, r)
	}), nil
}
