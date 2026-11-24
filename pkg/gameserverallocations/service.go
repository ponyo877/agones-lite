package gameserverallocations

import (
	"context"
	"errors"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"time"

	"agones.dev/agones/pkg/allocation/converters"
	pb "agones.dev/agones/pkg/allocation/go"
	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	allocationv1 "agones.dev/agones/pkg/apis/allocation/v1"
	gwruntime "github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"

	litev1 "github.com/ponyo877/agones-lite/pkg/apis/lite/v1"
	"github.com/ponyo877/agones-lite/pkg/events"
	"github.com/ponyo877/agones-lite/pkg/gameservers"
	"github.com/ponyo877/agones-lite/pkg/store"
)

// attempts is how often an allocation is tried again after losing a race
// for its GameServer. Upstream: allocationRetry in allocator.go.
const attempts = 5

// errConflict is returned when every attempt lost its race. Upstream
// returns an error here too.
var errConflict = errors.New("could not update the selected GameServer")

// Service is the allocation API, in the shape of upstream's allocator
// service: the same gRPC service and the same REST path, built from the
// same generated code. It is the thin half of this package.
//
// Requests choose their GameServer one at a time, which takes no waiting:
// the store has the choice at once. They wait for the store's file side by
// side, and the file takes them in one write (store.Run). Upstream gets
// the same effect by batching requests and updating a hundred GameServers
// in parallel.
type Service struct {
	pb.UnimplementedAllocationServiceServer
	servers store.Typed[*agonesv1.GameServer]
	nodes   store.Typed[*litev1.Node]
	events  *events.Recorder
	now     func() time.Time
	// Pick chooses among n GameServers that are equally good, by giving a
	// number below n. It is random unless a test says otherwise.
	Pick func(n int) int
	// Observe, when set, is told how each request ended and how long it
	// took. It is how the metrics learn of allocations.
	Observe func(fleet, status, strategy string, took time.Duration)

	mu sync.Mutex // one allocation at a time
}

// NewService returns a Service working on s.
func NewService(s *store.Store, recorder *events.Recorder) *Service {
	return &Service{
		servers: store.GameServersOf(s),
		nodes:   store.NodesOf(s),
		events:  recorder,
		now:     s.Now,
		Pick:    rand.IntN,
	}
}

// Allocate implements the allocation call. The request is prepared in
// upstream's order: defaults, validation, then the conversion of the
// deprecated selector fields.
func (s *Service) Allocate(_ context.Context, in *pb.AllocationRequest) (*pb.AllocationResponse, error) {
	gsa := converters.ConvertAllocationRequestToGSA(in)
	gsa.ApplyDefaults()
	if errs := gsa.Validate(); len(errs) > 0 {
		return nil, status.Error(codes.InvalidArgument, errs.ToAggregate().Error())
	}
	gsa.Converter()
	if gsa.Spec.MultiClusterSetting.Enabled {
		return nil, status.Error(codes.Unimplemented, "agones-lite does not allocate across clusters")
	}
	started := s.now()
	result, err := s.allocate(gsa)
	if err == nil {
		// Players are sent on this answer, so it waits for the file. The
		// next request does not: see allocate.
		err = s.servers.Sync()
	}
	s.observe(gsa, result, s.now().Sub(started))
	if err != nil {
		return nil, status.Error(codes.Aborted, err.Error())
	}
	return converters.ConvertGSAToAllocationResponse(result, codes.ResourceExhausted)
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
func (s *Service) allocate(gsa *allocationv1.GameServerAllocation) (*allocationv1.GameServerAllocation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for range attempts {
		all := s.servers.View("")
		gs, found := Find(gsa, s.reachable(gsa.Namespace, all), gameservers.CountByNode(all), s.Pick)
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

// reachable returns the GameServers of a namespace that sit on a node
// players can be sent to: connected and not cordoned.
func (s *Service) reachable(namespace string, all []*agonesv1.GameServer) []*agonesv1.GameServer {
	open := map[string]bool{}
	for _, node := range s.nodes.View("") {
		open[node.Name] = node.Schedulable()
	}
	var out []*agonesv1.GameServer
	for _, gs := range all {
		if gs.Namespace == namespace && open[gs.Status.NodeName] {
			out = append(out, gs)
		}
	}
	return out
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
