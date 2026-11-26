package agent

import (
	"context"
	"crypto/subtle"
	"log/slog"
	"maps"
	"net"
	"strings"
	"sync"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"k8s.io/apimachinery/pkg/types"

	"github.com/ponyo877/scrim/pkg/docker"
	"github.com/ponyo877/scrim/pkg/gameservers"
	"github.com/ponyo877/scrim/pkg/podspec"
	sidecarv1 "github.com/ponyo877/scrim/proto/sidecar/v1"
)

// sidecarServer answers the sidecars of the game servers on this node.
// The protocol is described in proto/sidecar/v1/sidecar.proto.
type sidecarServer struct {
	sidecarv1.UnimplementedAgentServer
	cp ControlPlane

	mu      sync.Mutex
	tickets map[Key]ticket                   // what lets each sidecar in
	views   map[Key]view                     // each GameServer as the SDK shows it
	streams map[Key]map[chan []byte]struct{} // open Watch calls
}

// ticket lets one sidecar in: the secret it must show, and the GameServer
// its containers were made for. A GameServer made later under the same
// name is another one. Its sidecar gets another ticket, and until then the
// old sidecar, should it still run, speaks for nobody.
type ticket struct {
	token string
	uid   types.UID
}

// view is a serialized sdk.GameServer and the resourceVersion it was made from.
type view struct {
	version string
	data    []byte
}

func newSidecarServer(cp ControlPlane) *sidecarServer {
	return &sidecarServer{
		cp:      cp,
		tickets: map[Key]ticket{},
		views:   map[Key]view{},
		streams: map[Key]map[chan []byte]struct{}{},
	}
}

func (s *sidecarServer) serve(ctx context.Context, listener net.Listener) {
	server := grpc.NewServer()
	sidecarv1.RegisterAgentServer(server, s)
	go func() {
		<-ctx.Done()
		server.Stop()
	}()
	if err := server.Serve(listener); err != nil && ctx.Err() == nil {
		slog.Error("sidecar listener stopped", "error", err)
	}
}

// allow lets a sidecar that is about to start connect.
func (s *sidecarServer) allow(key Key, t ticket) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tickets[key] = t
}

// update takes in the result of one reconcile pass. Tickets are read back
// from container labels, so they survive a restart of the agent.
func (s *sidecarServer) update(desired []*agonesv1.GameServer, actual []docker.Container) {
	s.mu.Lock()
	defer s.mu.Unlock()
	wanted := map[Key]types.UID{}
	for _, gs := range desired {
		wanted[Key{Namespace: gs.Namespace, Name: gs.Name}] = gs.UID
		s.refresh(gs)
	}
	for _, c := range actual {
		key := Key{Namespace: c.Labels[podspec.LabelNamespace], Name: c.Labels[podspec.LabelGameServer]}
		if token := c.Labels[podspec.LabelToken]; token != "" {
			s.tickets[key] = ticket{token: token, uid: types.UID(c.Labels[podspec.LabelUID])}
		}
	}
	s.forget(wanted)
}

// forget drops what is kept for GameServers that are not placed here any
// more. An agent runs for months, and would otherwise keep something for
// every GameServer it ever had.
//
// The ticket of a sidecar goes with its GameServer, also while its
// containers are still there: what it says would be passed on under a name
// that may by now belong to another GameServer, here or on another node.
// Called with the lock held.
func (s *sidecarServer) forget(wanted map[Key]types.UID) {
	maps.DeleteFunc(s.tickets, func(key Key, t ticket) bool { return wanted[key] != t.uid })
	maps.DeleteFunc(s.views, func(key Key, _ view) bool { _, placed := wanted[key]; return !placed })
}

// refresh publishes gs to its watchers if it changed. Called with the lock held.
func (s *sidecarServer) refresh(gs *agonesv1.GameServer) {
	key := Key{Namespace: gs.Namespace, Name: gs.Name}
	if s.views[key].version == gs.ResourceVersion {
		return
	}
	data, err := proto.Marshal(sdkView(gs))
	if err != nil {
		slog.Error("cannot serialize GameServer", "gameserver", key.String(), "error", err)
		return
	}
	s.views[key] = view{version: gs.ResourceVersion, data: data}
	for stream := range s.streams[key] {
		offer(stream, data)
	}
}

// offer replaces whatever the reader has not picked up yet: only the latest
// state matters. The channel holds one item and has one writer at a time.
func offer(stream chan []byte, data []byte) {
	select {
	case <-stream:
	default:
	}
	stream <- data
}

// Watch implements the Watch call of the protocol.
func (s *sidecarServer) Watch(_ *sidecarv1.WatchRequest, stream grpc.ServerStreamingServer[sidecarv1.WatchResponse]) error {
	key, _, err := s.authenticate(stream.Context())
	if err != nil {
		return err
	}
	updates, cancel := s.subscribe(key)
	defer cancel()
	for {
		select {
		case <-stream.Context().Done():
			return nil
		case data := <-updates:
			if err := stream.Send(&sidecarv1.WatchResponse{GameServer: data}); err != nil {
				return err
			}
		}
	}
}

// subscribe returns a channel that starts with the current view, if any.
func (s *sidecarServer) subscribe(key Key) (<-chan []byte, func()) {
	stream := make(chan []byte, 1)
	s.mu.Lock()
	defer s.mu.Unlock()
	if current, ok := s.views[key]; ok {
		stream <- current.data
	}
	if s.streams[key] == nil {
		s.streams[key] = map[chan []byte]struct{}{}
	}
	s.streams[key][stream] = struct{}{}
	return stream, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.streams[key], stream)
		if len(s.streams[key]) == 0 {
			delete(s.streams, key) // or the map would grow with every GameServer
		}
	}
}

// sdkEvents maps what a sidecar tells us to the control plane's Events.
var sdkEvents = map[sidecarv1.Event]gameservers.Event{
	sidecarv1.Event_EVENT_READY:           gameservers.SDKReady,
	sidecarv1.Event_EVENT_ALLOCATE:        gameservers.SDKAllocate,
	sidecarv1.Event_EVENT_SHUTDOWN:        gameservers.SDKShutdown,
	sidecarv1.Event_EVENT_RESERVE:         gameservers.SDKReserve,
	sidecarv1.Event_EVENT_HEALTH_FAILED:   gameservers.HealthFailed,
	sidecarv1.Event_EVENT_RESERVE_EXPIRED: gameservers.ReserveExpired,
}

// Tell implements the Tell call of the protocol.
func (s *sidecarServer) Tell(ctx context.Context, req *sidecarv1.TellRequest) (*sidecarv1.TellResponse, error) {
	key, uid, err := s.authenticate(ctx)
	if err != nil {
		return nil, err
	}
	if req.GetEvent() == sidecarv1.Event_EVENT_UPDATE_COUNTS {
		return s.updateCounts(ctx, key, req)
	}
	if err := s.pass(ctx, key, uid, req); err != nil {
		return nil, refusal(err)
	}
	return &sidecarv1.TellResponse{}, nil
}

// refusal is the answer to a request that could not be passed on. The
// sidecar tries again after any error but InvalidArgument, so that code is
// kept for the requests that will never work, and kept apart from the rest.
func refusal(err error) error {
	if status.Code(err) == codes.InvalidArgument {
		return err
	}
	return status.Error(codes.Internal, err.Error())
}

// updateCounts hands changes to Counters and Lists on to the control plane
// and answers with the GameServer as it is afterwards. The sidecar shows the
// game server its own prediction until this answer arrives.
func (s *sidecarServer) updateCounts(ctx context.Context, key Key, req *sidecarv1.TellRequest) (*sidecarv1.TellResponse, error) {
	gs, err := s.cp.UpdateCounts(ctx, key.Namespace, key.Name, req.GetChangeId(), req.GetCounters(), req.GetLists())
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if gs == nil {
		return &sidecarv1.TellResponse{}, nil // gone: there is nothing to show
	}
	data, err := proto.Marshal(sdkView(gs))
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &sidecarv1.TellResponse{GameServer: data}, nil
}

// pass hands the request on to the control plane.
func (s *sidecarServer) pass(ctx context.Context, key Key, uid types.UID, req *sidecarv1.TellRequest) error {
	if req.GetEvent() == sidecarv1.Event_EVENT_SET_METADATA {
		return s.cp.SetMetadata(ctx, key.Namespace, key.Name, req.GetLabels(), req.GetAnnotations())
	}
	event, ok := sdkEvents[req.GetEvent()]
	if !ok {
		return status.Errorf(codes.InvalidArgument, "unknown event %v", req.GetEvent())
	}
	return s.cp.Report(ctx, gameservers.Report{
		Namespace: key.Namespace,
		Name:      key.Name,
		UID:       uid,
		Event:     event,
		Reserve:   sidecarv1.ReserveFor(req.GetReserveSeconds()),
		ID:        req.GetChangeId(),
	})
}

// authenticate works out which GameServer is calling and checks its token.
// A game container shares the sidecar's network and could dial the agent
// too; it does not know the token, so it cannot pose as the sidecar.
func (s *sidecarServer) authenticate(ctx context.Context) (Key, types.UID, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	namespace, name, _ := strings.Cut(first(md, "gameserver"), "/")
	token := strings.TrimPrefix(first(md, "authorization"), "Bearer ")
	key := Key{Namespace: namespace, Name: name}

	s.mu.Lock()
	want, known := s.tickets[key]
	s.mu.Unlock()
	if !known || subtle.ConstantTimeCompare([]byte(token), []byte(want.token)) != 1 {
		return Key{}, "", status.Error(codes.Unauthenticated, "unknown game server or wrong token")
	}
	return key, want.uid, nil
}

func first(md metadata.MD, name string) string {
	if values := md.Get(name); len(values) > 0 {
		return values[0]
	}
	return ""
}
