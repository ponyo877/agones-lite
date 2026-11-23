// Command sidecar is the SDK server that runs beside one game server.
//
// The agent starts it in its own container and passes everything it needs
// through the environment. It imports no Kubernetes libraries, so it stays
// small: one runs for every game server.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/ponyo877/agones-lite/pkg/sdkserver"
)

// The environment variables, set by the agent (see pkg/podspec).
const (
	envAgent      = "AGONES_LITE_AGENT"
	envGameServer = "AGONES_LITE_GAMESERVER"
	envToken      = "AGONES_LITE_TOKEN"
	envGRPCPort   = "AGONES_SDK_GRPC_PORT"
	envHTTPPort   = "AGONES_SDK_HTTP_PORT"
)

func main() {
	if err := run(); err != nil {
		slog.Error("sidecar stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	grpcPort, err := port(envGRPCPort, 9357)
	if err != nil {
		return err
	}
	httpPort, err := port(envHTTPPort, 9358)
	if err != nil {
		return err
	}
	creds := sdkserver.Credentials{GameServer: os.Getenv(envGameServer), Token: os.Getenv(envToken)}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	slog.Info("sidecar started", "gameserver", creds.GameServer, "grpc", grpcPort, "http", httpPort)
	return sdkserver.Serve(ctx, os.Getenv(envAgent), creds, grpcPort, httpPort)
}

// port reads a port number from the environment. Unset or zero means fallback.
func port(name string, fallback int) (int, error) {
	value := os.Getenv(name)
	if value == "" || value == "0" {
		return fallback, nil
	}
	n, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("%s=%q is not a port number", name, value)
	}
	return n, nil
}
