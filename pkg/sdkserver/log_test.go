package sdkserver

import (
	"bytes"
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// With spec.sdkServer.logLevel: Debug, each call is noted with what was
// called, and answered as before.
func TestSDKCallsAreNotedAtDebug(t *testing.T) {
	var out bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	answer, err := logCall(context.Background(), "request", &grpc.UnaryServerInfo{FullMethod: "/agones.dev.sdk.SDK/Ready"},
		func(context.Context, any) (any, error) { return "answer", nil })

	require.NoError(t, err)
	assert.Equal(t, "answer", answer)
	assert.Contains(t, out.String(), "method=/agones.dev.sdk.SDK/Ready")
}
