package nodes

import (
	"context"
	"encoding/json"
	"io"
	"slices"
	"sync"
	"time"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	litev1 "github.com/ponyo877/agones-lite/pkg/apis/lite/v1"
	"github.com/ponyo877/agones-lite/pkg/gameservers"
	"github.com/ponyo877/agones-lite/pkg/store"
	nodev1 "github.com/ponyo877/agones-lite/proto/node/v1"
	sidecarv1 "github.com/ponyo877/agones-lite/proto/sidecar/v1"
)

// reportable lists the Events an agent may report. The others are the
// control plane's own decisions and are not accepted from outside.
var reportable = []gameservers.Event{
	gameservers.ContainersCreated, gameservers.CreateFailed, gameservers.ContainersRunning,
	gameservers.ExitedCleanly, gameservers.ExitedWithError,
	gameservers.ContainersMissing, gameservers.ContainersRemoved,
	gameservers.SDKReady, gameservers.SDKAllocate, gameservers.SDKReserve,
	gameservers.SDKShutdown, gameservers.HealthFailed, gameservers.ReserveExpired,
}

// errFellBehind ends the Watch call of an agent that the store stopped
// waiting for. Changes were lost on the way to it, so its copy cannot be
// mended: the error makes the agent call again and receive everything anew.
var errFellBehind = status.Error(codes.Aborted, "fell too far behind the changes; watch again")

// Server answers the agents on other machines. The protocol is described in
// proto/node/v1/node.proto; the work is done by a Local.
type Server struct {
	nodev1.UnimplementedNodesServer
	local   *Local
	servers store.Typed[*agonesv1.GameServer]
	nodes   store.Typed[*litev1.Node]
	now     func() time.Time

	mu      sync.Mutex
	streams map[string]int            // open Watch calls per node
	logs    map[string]*io.PipeWriter // logs that were asked for, by request id
}

// NewServer returns a Server that works through local.
func NewServer(s *store.Store, local *Local) *Server {
	return &Server{
		local:   local,
		servers: store.GameServersOf(s),
		nodes:   store.NodesOf(s),
		now:     s.Now,
		streams: map[string]int{},
		logs:    map[string]*io.PipeWriter{},
	}
}

// Register implements the Register call of the protocol.
//
// Whatever the agent says of itself, its node is only as connected as its
// Watch calls: the end of the last one is how we learn that the agent is
// gone. Were a Register to make the node Ready with no call open, nothing
// would ever make it Disconnected again.
func (s *Server) Register(ctx context.Context, req *nodev1.RegisterRequest) (*nodev1.RegisterResponse, error) {
	node := &litev1.Node{}
	if err := json.Unmarshal(req.GetNode(), node); err != nil || node.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "the request must carry a Node with a name")
	}
	s.mu.Lock()
	defer s.mu.Unlock() // held to the end, so no Watch call opens or ends meanwhile
	node.Status.Phase = s.phase(node.Name)
	if err := s.local.Register(ctx, node); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &nodev1.RegisterResponse{}, nil
}

// Watch implements the Watch call of the protocol. The node counts as
// connected for as long as the call lasts.
func (s *Server) Watch(req *nodev1.WatchRequest, stream grpc.ServerStreamingServer[nodev1.WatchResponse]) error {
	node := req.GetNode()
	// Subscribe before listing, so a change in between is not missed.
	// It may then be sent twice, which is harmless.
	changes, cancel := s.servers.Watch()
	defer cancel()
	s.connected(node, +1)
	defer s.connected(node, -1)
	logs := &remoteLogs{server: s, requests: make(chan *nodev1.LogRequest)}
	s.local.Attach(node, logs)
	defer s.local.Detach(node, logs)

	if err := s.sendPlaced(stream, node); err != nil {
		return err
	}
	for {
		var err error
		select {
		case <-stream.Context().Done():
			return nil
		case change, open := <-changes:
			if !open {
				return errFellBehind
			}
			err = forward(stream, node, change)
		case request := <-logs.requests:
			err = stream.Send(&nodev1.WatchResponse{Change: &nodev1.WatchResponse_LogRequest{LogRequest: request}})
		}
		if err != nil {
			return err
		}
	}
}

// sendPlaced sends everything that is on the node now, then says so.
func (s *Server) sendPlaced(stream grpc.ServerStreamingServer[nodev1.WatchResponse], node string) error {
	placed, _ := s.local.Assigned(node)
	for _, gs := range placed {
		if err := sendGameServer(stream, gs); err != nil {
			return err
		}
	}
	return stream.Send(&nodev1.WatchResponse{Change: &nodev1.WatchResponse_Synced{Synced: &nodev1.Synced{}}})
}

// forward passes on a change if it concerns a GameServer on the node.
func forward(stream grpc.ServerStreamingServer[nodev1.WatchResponse], node string, change store.Event) error {
	gs, ok := change.Object.(*agonesv1.GameServer)
	if !ok || gs.Status.NodeName != node {
		return nil
	}
	if change.Type == store.Deleted {
		removed := &nodev1.Removed{Namespace: gs.Namespace, Name: gs.Name}
		return stream.Send(&nodev1.WatchResponse{Change: &nodev1.WatchResponse_Removed{Removed: removed}})
	}
	return sendGameServer(stream, gs)
}

func sendGameServer(stream grpc.ServerStreamingServer[nodev1.WatchResponse], gs *agonesv1.GameServer) error {
	data, err := json.Marshal(gs)
	if err != nil {
		return err
	}
	return stream.Send(&nodev1.WatchResponse{Change: &nodev1.WatchResponse_GameServer{GameServer: data}})
}

// connected counts a node's open Watch calls and records when the node
// connects with its first and disconnects with its last. A reconnecting
// agent may briefly hold two.
func (s *Server) connected(node string, delta int) {
	s.mu.Lock()
	defer s.mu.Unlock() // held to the end, like Register
	s.streams[node] += delta
	phase := s.phase(node)
	_ = s.nodes.Change("", node, func(n *litev1.Node) bool {
		if n.Status.Phase == phase {
			return false
		}
		n.Status.Phase = phase
		// For a disconnected node this is the moment it was last seen.
		n.Status.LastHeartbeatTime = metav1.NewTime(s.now())
		return true
	})
}

// phase is what a node's open Watch calls say about it. Called with the lock held.
func (s *Server) phase(node string) litev1.NodePhase {
	if s.streams[node] > 0 {
		return litev1.NodeReady
	}
	return litev1.NodeDisconnected
}

// Report implements the Report call of the protocol.
func (s *Server) Report(ctx context.Context, req *nodev1.ReportRequest) (*nodev1.ReportResponse, error) {
	event := gameservers.Event(req.GetEvent())
	if !slices.Contains(reportable, event) {
		return nil, status.Errorf(codes.InvalidArgument, "agents cannot report %q", req.GetEvent())
	}
	err := s.local.Report(ctx, gameservers.Report{
		Namespace: req.GetNamespace(),
		Name:      req.GetName(),
		UID:       types.UID(req.GetUid()),
		Event:     event,
		Reserve:   sidecarv1.ReserveFor(req.GetReserveSeconds()),
		Detail:    req.GetDetail(),
	})
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &nodev1.ReportResponse{}, nil
}

// UpdateCounts implements the UpdateCounts call of the protocol.
func (s *Server) UpdateCounts(ctx context.Context, req *nodev1.UpdateCountsRequest) (*nodev1.UpdateCountsResponse, error) {
	gs, err := s.local.UpdateCounts(ctx, req.GetNamespace(), req.GetName(), req.GetChangeId(), req.GetCounters(), req.GetLists())
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if gs == nil {
		return &nodev1.UpdateCountsResponse{}, nil
	}
	data, err := json.Marshal(gs)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &nodev1.UpdateCountsResponse{GameServer: data}, nil
}

// SetMetadata implements the SetMetadata call of the protocol.
func (s *Server) SetMetadata(ctx context.Context, req *nodev1.SetMetadataRequest) (*nodev1.SetMetadataResponse, error) {
	if err := s.local.SetMetadata(ctx, req.GetNamespace(), req.GetName(), req.GetLabels(), req.GetAnnotations()); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &nodev1.SetMetadataResponse{}, nil
}
