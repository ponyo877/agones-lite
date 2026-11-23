package agent

import (
	"context"
	"crypto/subtle"
	"log/slog"
	"math"
	"net"
	"strings"
	"sync"
	"time"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/ponyo877/agones-lite/pkg/docker"
	"github.com/ponyo877/agones-lite/pkg/gameservers"
	"github.com/ponyo877/agones-lite/pkg/podspec"
	sidecarv1 "github.com/ponyo877/agones-lite/proto/sidecar/v1"
)

// sidecarServer answers the sidecars of the game servers on this node.
// The protocol is described in proto/sidecar/v1/sidecar.proto.
type sidecarServer struct {
	sidecarv1.UnimplementedAgentServer
	cp ControlPlane

	mu      sync.Mutex
	tokens  map[Key]string                   // the secret each sidecar must show
	views   map[Key]view                     // each GameServer as the SDK shows it
	streams map[Key]map[chan []byte]struct{} // open Watch calls
}

// view is a serialized sdk.GameServer and the resourceVersion it was made from.
type view struct {
	version string
	data    []byte
}

func newSidecarServer(cp ControlPlane) *sidecarServer {
	return &sidecarServer{
		cp:      cp,
		tokens:  map[Key]string{},
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

// allow lets a sidecar that is about to start connect with token.
func (s *sidecarServer) allow(key Key, token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens[key] = token
}

// update takes in the result of one reconcile pass. Tokens are read back
// from container labels, so they survive a restart of the agent.
func (s *sidecarServer) update(desired []*agonesv1.GameServer, actual []docker.Container) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range actual {
		if token := c.Labels[podspec.LabelToken]; token != "" {
			s.tokens[Key{Namespace: c.Labels[podspec.LabelNamespace], Name: c.Labels[podspec.LabelGameServer]}] = token
		}
	}
	for _, gs := range desired {
		s.refresh(gs)
	}
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
	key, err := s.authenticate(stream.Context())
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
	}
}

// sdkEvents maps what a sidecar tells us to the control plane's Events.
var sdkEvents = map[sidecarv1.Event]gameservers.Event{
	sidecarv1.Event_EVENT_READY:         gameservers.SDKReady,
	sidecarv1.Event_EVENT_ALLOCATE:      gameservers.SDKAllocate,
	sidecarv1.Event_EVENT_SHUTDOWN:      gameservers.SDKShutdown,
	sidecarv1.Event_EVENT_RESERVE:       gameservers.SDKReserve,
	sidecarv1.Event_EVENT_HEALTH_FAILED: gameservers.HealthFailed,
}

// Tell implements the Tell call of the protocol.
func (s *sidecarServer) Tell(ctx context.Context, req *sidecarv1.TellRequest) (*sidecarv1.TellResponse, error) {
	key, err := s.authenticate(ctx)
	if err != nil {
		return nil, err
	}
	if req.GetEvent() == sidecarv1.Event_EVENT_UPDATE_COUNTS {
		return s.updateCounts(key, req)
	}
	if err := s.pass(key, req); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &sidecarv1.TellResponse{}, nil
}

// updateCounts hands changes to Counters and Lists on to the control plane
// and answers with the GameServer as it is afterwards. The sidecar shows the
// game server its own prediction until this answer arrives.
func (s *sidecarServer) updateCounts(key Key, req *sidecarv1.TellRequest) (*sidecarv1.TellResponse, error) {
	gs, err := s.cp.UpdateCounts(key.Namespace, key.Name, req.GetChangeId(), req.GetCounters(), req.GetLists())
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
func (s *sidecarServer) pass(key Key, req *sidecarv1.TellRequest) error {
	if req.GetEvent() == sidecarv1.Event_EVENT_SET_METADATA {
		return s.cp.SetMetadata(key.Namespace, key.Name, req.GetLabels(), req.GetAnnotations())
	}
	event, ok := sdkEvents[req.GetEvent()]
	if !ok {
		return status.Errorf(codes.InvalidArgument, "unknown event %v", req.GetEvent())
	}
	return s.cp.Report(gameservers.Report{
		Namespace: key.Namespace,
		Name:      key.Name,
		Event:     event,
		Reserve:   reserveFor(req.GetReserveSeconds()),
	})
}

// reserveFor turns the seconds of a Reserve into a Duration. More of them
// than a Duration holds (292 years) would wrap around into a short or a
// negative time, so they count as the longest one. Zero or less means
// "until told otherwise".
func reserveFor(seconds int64) time.Duration {
	const longest = int64(math.MaxInt64 / time.Second)
	return time.Duration(min(max(seconds, 0), longest)) * time.Second
}

// authenticate works out which GameServer is calling and checks its token.
// A game container shares the sidecar's network and could dial the agent
// too; it does not know the token, so it cannot pose as the sidecar.
func (s *sidecarServer) authenticate(ctx context.Context) (Key, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	namespace, name, _ := strings.Cut(first(md, "gameserver"), "/")
	token := strings.TrimPrefix(first(md, "authorization"), "Bearer ")
	key := Key{Namespace: namespace, Name: name}

	s.mu.Lock()
	want, known := s.tokens[key]
	s.mu.Unlock()
	if !known || subtle.ConstantTimeCompare([]byte(token), []byte(want)) != 1 {
		return Key{}, status.Error(codes.Unauthenticated, "unknown game server or wrong token")
	}
	return key, nil
}

func first(md metadata.MD, name string) string {
	if values := md.Get(name); len(values) > 0 {
		return values[0]
	}
	return ""
}
