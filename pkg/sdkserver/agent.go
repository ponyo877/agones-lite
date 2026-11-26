package sdkserver

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"time"

	"agones.dev/agones/pkg/sdk"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	sidecarv1 "github.com/ponyo877/scrim/proto/sidecar/v1"
)

// retryDelay is how long to wait before reaching for the agent again.
const retryDelay = time.Second

// Credentials names this sidecar's GameServer and proves it is the sidecar.
// It is attached to every call to the agent.
type Credentials struct {
	GameServer string // namespace/name
	Token      string
}

// GetRequestMetadata implements credentials.PerRPCCredentials.
func (c Credentials) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"gameserver": c.GameServer, "authorization": "Bearer " + c.Token}, nil
}

// RequireTransportSecurity implements credentials.PerRPCCredentials. The
// agent is on the same machine, so the connection is not encrypted.
func (Credentials) RequireTransportSecurity() bool {
	return false
}

// tell queues a request for the agent and returns at once. The SDK contract
// is that these calls are acknowledged before they take effect.
//
// The request gets a name of its own. Should its answer get lost, it is sent
// again (send), and the control plane must not take it for a second one.
func (s *Server) tell(req *sidecarv1.TellRequest) (*sdk.Empty, error) {
	req.ChangeId = rand.Text()
	select {
	case s.outbox <- req:
		return &sdk.Empty{}, nil
	default:
		return nil, status.Error(codes.Unavailable, "too many requests are waiting for the agent")
	}
}

// Serve is the whole sidecar: it dials the agent at address, introduces
// itself with creds, and serves the SDK on the two ports until ctx is done.
func Serve(ctx context.Context, address string, creds Credentials, grpcPort, httpPort int) error {
	conn, err := grpc.NewClient(address,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithPerRPCCredentials(creds),
	)
	if err != nil {
		return fmt.Errorf("agent address %q: %w", address, err)
	}
	defer func() { _ = conn.Close() }()
	return New(sidecarv1.NewAgentClient(conn)).Run(ctx, grpcPort, httpPort)
}

// deliver sends queued requests to the agent in order. A request that cannot
// be sent is retried until it can: the agent may be restarting.
func (s *Server) deliver(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case req := <-s.outbox:
			s.send(ctx, req)
		}
	}
}

// send hands one request to the agent, however many tries that takes, and
// takes in the agent's answer.
//
// One kind of failure is not tried again: a request the agent says it
// cannot make sense of, such as an event from a sidecar newer than itself.
// It will not make sense of it later either, and everything queued behind
// it would wait for ever.
func (s *Server) send(ctx context.Context, req *sidecarv1.TellRequest) {
	for ctx.Err() == nil {
		resp, err := s.agent.Tell(ctx, req)
		if err == nil {
			s.settle(req, answer(resp))
			return
		}
		if refusedForGood(err) {
			slog.Error("the agent refuses the request; dropping it", "event", req.GetEvent().String(), "error", err)
			s.settle(req, nil)
			return
		}
		slog.Warn("agent did not take the request, retrying", "event", req.GetEvent().String(), "error", err)
		sleep(ctx, retryDelay)
	}
}

func refusedForGood(err error) bool {
	code := status.Code(err)
	return code == codes.InvalidArgument || code == codes.Unimplemented
}

// answer reads the GameServer the agent sends back after it changed Counters
// or Lists. Other requests are answered without one, which gives nil.
func answer(resp *sidecarv1.TellResponse) *sdk.GameServer {
	if len(resp.GetGameServer()) == 0 {
		return nil
	}
	gs := &sdk.GameServer{}
	if err := proto.Unmarshal(resp.GetGameServer(), gs); err != nil {
		// The next GameServer the agent pushes will put the view right.
		slog.Warn("cannot read the agent's answer", "error", err)
		return nil
	}
	return gs
}

// follow keeps the local copy of the GameServer up to date, reconnecting
// whenever the stream breaks.
func (s *Server) follow(ctx context.Context) {
	for ctx.Err() == nil {
		err := s.followOnce(ctx) // only returns when the stream broke
		if ctx.Err() == nil {
			slog.Warn("lost the agent, reconnecting", "error", err)
		}
		sleep(ctx, retryDelay)
	}
}

func (s *Server) followOnce(ctx context.Context) error {
	stream, err := s.agent.Watch(ctx, &sidecarv1.WatchRequest{})
	if err != nil {
		return err
	}
	for {
		update, err := stream.Recv()
		if err != nil {
			return err
		}
		gs := &sdk.GameServer{}
		if err := proto.Unmarshal(update.GetGameServer(), gs); err != nil {
			return err
		}
		s.set(gs)
	}
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
