// Package sdkserver is the Agones SDK server that runs beside every game
// server, in the sidecar container.
//
// A game server built with any official Agones SDK talks to this package and
// cannot tell it from the real thing: the gRPC and REST code is generated
// from Agones' own definitions (agones.dev/agones/pkg/sdk).
//
// The sidecar keeps one copy of its GameServer, fed by the node's agent, and
// answers reads from it. Requests that change something are queued for the
// agent and acknowledged at once, as upstream does. A change to a Counter or
// a List also shows in every read from that moment on.
//
//   - server.go: the state and the two listeners (gRPC and REST)
//   - sdk.go: the stable SDK calls
//   - counts.go: the beta SDK calls, Counters and Lists, and what they refuse
//   - view.go: the GameServer as the game server sees it, changes on their
//     way to the agent included
//   - pending.go: what such a change does to a GameServer
//   - agent.go: the link to the agent
//   - health.go: noticing a game server that stopped sending Health pings
//   - stages.go: the alpha SDK service, which is switched off
//
// Upstream counterpart: pkg/sdkserver and cmd/sdk-server.
package sdkserver

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"agones.dev/agones/pkg/sdk"
	sdkalpha "agones.dev/agones/pkg/sdk/alpha"
	sdkbeta "agones.dev/agones/pkg/sdk/beta"
	gwruntime "github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/tmc/grpc-websocket-proxy/wsproxy"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/encoding/protojson"

	sidecarv1 "github.com/ponyo877/agones-lite/proto/sidecar/v1"
)

// Server implements the stable Agones SDK service.
type Server struct {
	sdk.UnimplementedSDKServer
	agent  sidecarv1.AgentClient
	outbox chan *sidecarv1.TellRequest // requests on their way to the agent
	health *health

	mu       sync.Mutex
	current  *sdk.GameServer          // the base: the last GameServer from the agent
	pending  []*sidecarv1.TellRequest // changes to Counters and Lists the agent has not answered, oldest first
	held     *sdk.GameServer          // pushed while changes were pending; see set
	view     *sdk.GameServer          // current with pending on top: what the game server sees
	arrived  chan struct{}            // closed once the first GameServer came in
	watchers map[chan *sdk.GameServer]struct{}
	reserve  *time.Timer // returns a Reserved game server to Ready
}

// New returns a Server that talks to the agent through client.
func New(client sidecarv1.AgentClient) *Server {
	return &Server{
		agent:    client,
		outbox:   make(chan *sidecarv1.TellRequest, 64),
		health:   &health{},
		arrived:  make(chan struct{}),
		watchers: map[chan *sdk.GameServer]struct{}{},
	}
}

// Run serves the SDK on the loopback interface until ctx is done. Game
// servers reach it there because they share the sidecar's network.
func (s *Server) Run(ctx context.Context, grpcPort, httpPort int) error {
	go s.follow(ctx)
	go s.deliver(ctx)
	go s.watchHealth(ctx)

	grpcListeners, err := loopback(grpcPort)
	if err != nil {
		return err
	}
	httpListeners, err := loopback(httpPort)
	if err != nil {
		return err
	}
	grpcServer := grpc.NewServer()
	sdk.RegisterSDKServer(grpcServer, s)
	sdkbeta.RegisterSDKServer(grpcServer, beta{server: s})
	sdkalpha.RegisterSDKServer(grpcServer, alpha{})

	gateway, err := newGateway(ctx, fmt.Sprintf("127.0.0.1:%d", grpcPort))
	if err != nil {
		return err
	}
	httpServer := &http.Server{Handler: gateway, ReadHeaderTimeout: 10 * time.Second}

	failed := make(chan error, len(grpcListeners)+len(httpListeners))
	for _, l := range grpcListeners {
		go func() { failed <- grpcServer.Serve(l) }()
	}
	for _, l := range httpListeners {
		go func() { failed <- httpServer.Serve(l) }()
	}
	select {
	case <-ctx.Done():
		grpcServer.Stop()
		_ = httpServer.Close()
		return nil
	case err := <-failed:
		return err
	}
}

// loopback listens on both loopback addresses. Upstream listens on
// "localhost", which Go turns into a single address; clients that resolve
// localhost to the other one are then refused. Answering on both costs nothing.
func loopback(port int) ([]net.Listener, error) {
	v4, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, err
	}
	v6, err := net.Listen("tcp6", fmt.Sprintf("[::1]:%d", port))
	if err != nil {
		return []net.Listener{v4}, nil //nolint:nilerr // IPv6 is switched off here, which is no reason not to serve
	}
	return []net.Listener{v4, v6}, nil
}

// newGateway returns the REST front of the gRPC server, set up exactly as
// upstream's (NewServerMux in pkg/util/runtime and cmd/sdk-server/main.go):
// proto field names in JSON, empty fields included, and WebSocket support.
func newGateway(ctx context.Context, grpcAddr string) (http.Handler, error) {
	mux := gwruntime.NewServeMux(
		gwruntime.WithMarshalerOption(gwruntime.MIMEWildcard, &gwruntime.HTTPBodyMarshaler{
			Marshaler: &gwruntime.JSONPb{
				MarshalOptions:   protojson.MarshalOptions{UseProtoNames: true, EmitUnpopulated: true},
				UnmarshalOptions: protojson.UnmarshalOptions{DiscardUnknown: true},
			},
		}),
	)
	conn, err := grpc.NewClient(grpcAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, err
	}
	register := []func(context.Context, *gwruntime.ServeMux, *grpc.ClientConn) error{
		sdk.RegisterSDKHandler, sdkbeta.RegisterSDKHandler, sdkalpha.RegisterSDKHandler,
	}
	for _, fn := range register {
		if err := fn(ctx, mux, conn); err != nil {
			return nil, err
		}
	}
	return wsproxy.WebsocketProxy(rejectEmptyHealth(mux)), nil
}

// rejectEmptyHealth mirrors upstream's healthCheckWrapper: a Health ping
// over REST must have a body.
func rejectEmptyHealth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" && r.Body == http.NoBody {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}
