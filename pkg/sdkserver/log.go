package sdkserver

import (
	"context"
	"log/slog"

	"google.golang.org/grpc"
)

// logCall notes each SDK call at Debug, the level upstream notes them at,
// so that spec.sdkServer.logLevel: Debug shows what the game server asks
// for. REST and WebSocket calls come through here too: the gateway makes
// them gRPC calls.
func logCall(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	slog.DebugContext(ctx, "SDK call", "method", info.FullMethod)
	return handler(ctx, req)
}

// logStream notes the start of each stream of the SDK: Health and
// WatchGameServer.
func logStream(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	slog.DebugContext(ss.Context(), "SDK stream", "method", info.FullMethod)
	return handler(srv, ss)
}
