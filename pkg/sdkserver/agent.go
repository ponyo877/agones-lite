package sdkserver

import (
	"context"
	"log/slog"
	"time"

	"agones.dev/agones/pkg/sdk"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	sidecarv1 "github.com/ponyo877/agones-lite/proto/sidecar/v1"
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
func (s *Server) tell(req *sidecarv1.TellRequest) (*sdk.Empty, error) {
	select {
	case s.outbox <- req:
		return &sdk.Empty{}, nil
	default:
		return nil, status.Error(codes.Unavailable, "too many requests are waiting for the agent")
	}
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

func (s *Server) send(ctx context.Context, req *sidecarv1.TellRequest) {
	for ctx.Err() == nil {
		_, err := s.agent.Tell(ctx, req)
		if err == nil {
			return
		}
		slog.Warn("agent did not take the request, retrying", "event", req.GetEvent().String(), "error", err)
		sleep(ctx, retryDelay)
	}
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
