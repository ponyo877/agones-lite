package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/ponyo877/agones-lite/pkg/agent"
	"github.com/ponyo877/agones-lite/pkg/agones"
	litev1 "github.com/ponyo877/agones-lite/pkg/apis/lite/v1"
	"github.com/ponyo877/agones-lite/pkg/apiserver"
	"github.com/ponyo877/agones-lite/pkg/docker"
	"github.com/ponyo877/agones-lite/pkg/gameservers"
	"github.com/ponyo877/agones-lite/pkg/nodes"
	"github.com/ponyo877/agones-lite/pkg/store"
)

const sidecarPort = "9447"

type serverOptions struct {
	dataDir    string
	listen     string
	dockerHost string
	agent      agent.Config
	minPort    int
	maxPort    int
	maxServers int
}

func serverFlags(args []string) (*serverOptions, error) {
	hostname, _ := os.Hostname()
	o := &serverOptions{}
	fs := flag.NewFlagSet("server", flag.ContinueOnError)
	fs.StringVar(&o.dataDir, "data-dir", "agones-lite-data", "directory for the state database")
	fs.StringVar(&o.listen, "listen", "127.0.0.1:9443", "address of the management API")
	fs.StringVar(&o.dockerHost, "docker-host", docker.DefaultHost(), "Docker daemon socket")
	fs.StringVar(&o.agent.NodeName, "node-name", hostname, "name of this node")
	fs.StringVar(&o.agent.Address, "address", "127.0.0.1", "address players use to reach game servers on this node")
	fs.IntVar(&o.minPort, "min-port", 7000, "first host port for game servers")
	fs.IntVar(&o.maxPort, "max-port", 8000, "last host port for game servers")
	fs.IntVar(&o.maxServers, "max-gameservers", 0, "most game servers on this node; 0 means no limit")
	fs.StringVar(&o.agent.SidecarImage, "sidecar-image", "agones-lite-sidecar:dev", "image of the SDK sidecar")
	fs.StringVar(&o.agent.SidecarListen, "sidecar-listen", "", "where the agent listens for sidecars (default: chosen per platform)")
	fs.StringVar(&o.agent.SidecarDial, "sidecar-dial", "host.docker.internal:"+sidecarPort, "how a container reaches --sidecar-listen")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	o.agent.PortRange = litev1.PortRange{Min: int32(o.minPort), Max: int32(o.maxPort)}
	o.agent.MaxGameServers = int32(o.maxServers)
	return o, nil
}

// runServer starts the control plane with an agent in the same process.
func runServer(ctx context.Context, args []string, out io.Writer) error {
	o, err := serverFlags(args)
	if err != nil {
		return err
	}
	if err := agones.Setup(); err != nil {
		return err
	}
	if err := os.MkdirAll(o.dataDir, 0o700); err != nil {
		return err
	}
	s, err := store.Open(filepath.Join(o.dataDir, "state.db"), store.Kinds())
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }()

	daemon, err := connectDocker(ctx, o)
	if err != nil {
		return err
	}
	controller := gameservers.NewController(s)
	node := agent.New(o.agent, nodes.NewLocal(s, controller), daemon)
	api := &http.Server{
		Addr:              o.listen,
		Handler:           apiserver.New(s, controller).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	_, _ = fmt.Fprintf(out, "agones-lite: API on http://%s, node %q, sidecars on %s\n", o.listen, o.agent.NodeName, o.agent.SidecarListen)
	return runAll(ctx,
		func(ctx context.Context) error { controller.Run(ctx); return nil },
		node.Run,
		func(ctx context.Context) error { return serve(ctx, api) },
	)
}

// connectDocker checks that the daemon answers and fills in where the agent
// should listen for sidecars.
func connectDocker(ctx context.Context, o *serverOptions) (*docker.Client, error) {
	daemon, err := docker.New(o.dockerHost)
	if err != nil {
		return nil, err
	}
	if err := daemon.Ping(ctx); err != nil {
		return nil, fmt.Errorf("cannot reach Docker at %s (set --docker-host or DOCKER_HOST): %w", o.dockerHost, err)
	}
	if o.agent.SidecarListen == "" {
		o.agent.SidecarListen, err = defaultSidecarListen(ctx, daemon)
	}
	return daemon, err
}

// defaultSidecarListen picks the address containers can reach the host on.
// On Linux that is the Docker bridge's gateway. On macOS Docker runs in a
// virtual machine that forwards to the host's loopback.
func defaultSidecarListen(ctx context.Context, daemon *docker.Client) (string, error) {
	if runtime.GOOS != "linux" {
		return "127.0.0.1:" + sidecarPort, nil
	}
	gateway, err := daemon.BridgeGateway(ctx)
	if err != nil {
		return "", err
	}
	return gateway + ":" + sidecarPort, nil
}

func serve(ctx context.Context, server *http.Server) error {
	go func() {
		<-ctx.Done()
		_ = server.Close()
	}()
	if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// runAll runs the parts of the server side by side. When one fails or ctx
// ends, the others are told to stop and runAll returns the first error.
func runAll(ctx context.Context, parts ...func(context.Context) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan error, len(parts))
	for _, part := range parts {
		go func() { results <- part(ctx) }()
	}
	var first error
	for range parts {
		if err := <-results; err != nil && first == nil {
			first = err
		}
		cancel()
	}
	return first
}
