package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/ponyo877/agones-lite/pkg/agent"
	"github.com/ponyo877/agones-lite/pkg/agones"
	litev1 "github.com/ponyo877/agones-lite/pkg/apis/lite/v1"
	"github.com/ponyo877/agones-lite/pkg/docker"
	"github.com/ponyo877/agones-lite/pkg/nodes"
	"github.com/ponyo877/agones-lite/pkg/version"
)

const sidecarPort = "9447"

// nodeOptions are the flags that describe a node. The `server` command has
// them for the agent it runs itself, and the `agent` command for its own.
type nodeOptions struct {
	dockerHost   string
	dockerConfig string
	agent        agent.Config
	minPort      int
	maxPort      int
	maxServers   int
}

func (n *nodeOptions) register(fs *flag.FlagSet) {
	hostname, _ := os.Hostname()
	fs.StringVar(&n.dockerHost, "docker-host", docker.DefaultHost(), "Docker daemon socket")
	fs.StringVar(&n.dockerConfig, "docker-config", defaultDockerConfig(), "Docker configuration file whose registry credentials are used to pull images; it need not exist")
	fs.StringVar(&n.agent.NodeName, "node-name", hostname, "name of this node")
	fs.StringVar(&n.agent.Address, "address", "127.0.0.1", "address players use to reach game servers on this node")
	fs.IntVar(&n.minPort, "min-port", 7000, "first host port for game servers")
	fs.IntVar(&n.maxPort, "max-port", 8000, "last host port for game servers")
	fs.IntVar(&n.maxServers, "max-gameservers", 0, "most game servers on this node; 0 means no limit")
	fs.StringVar(&n.agent.SidecarImage, "sidecar-image", version.SidecarImage(), "image of the SDK sidecar")
	fs.StringVar(&n.agent.SidecarListen, "sidecar-listen", "", "where the agent listens for sidecars: unix:///dir/agent.sock or host:port (default: chosen per platform)")
	fs.StringVar(&n.agent.SidecarDial, "sidecar-dial", "host.docker.internal:"+sidecarPort, "how a container reaches --sidecar-listen when that is host:port")
}

// defaultDockerConfig is where the docker command keeps the credentials
// "docker login" was given: $DOCKER_CONFIG/config.json, or else
// ~/.docker/config.json.
func defaultDockerConfig() string {
	if dir := os.Getenv("DOCKER_CONFIG"); dir != "" {
		return filepath.Join(dir, "config.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "" // no home, no credentials
	}
	return filepath.Join(home, ".docker", "config.json")
}

// finish copies the parsed numbers into the agent's configuration.
func (n *nodeOptions) finish() {
	n.agent.PortRange = litev1.PortRange{Min: int32(n.minPort), Max: int32(n.maxPort)}
	n.agent.MaxGameServers = int32(n.maxServers)
}

// connect checks the node's settings and that the Docker daemon answers,
// and fills in where the agent should listen for sidecars.
func (n *nodeOptions) connect(ctx context.Context) (*docker.Client, error) {
	if err := n.checkPorts(); err != nil {
		return nil, err
	}
	daemon, err := docker.New(n.dockerHost)
	if err != nil {
		return nil, err
	}
	daemon.ConfigFile = n.dockerConfig
	if err := daemon.Ping(ctx); err != nil {
		// Not there, or there with no API version this client speaks.
		return nil, fmt.Errorf("cannot use Docker at %s (see --docker-host and DOCKER_HOST): %w", n.dockerHost, err)
	}
	if n.agent.SidecarListen == "" {
		n.agent.SidecarListen, err = defaultSidecarListen(ctx, daemon, n.agent.NodeName)
	}
	return daemon, err
}

// checkPorts refuses a port range that holds no port. A node with one
// would register fine and then have no port for any game server.
func (n *nodeOptions) checkPorts() error {
	if n.minPort < 1 || n.maxPort > 65535 || n.minPort > n.maxPort {
		return fmt.Errorf("--min-port %d and --max-port %d are not a port range: need 1 <= min <= max <= 65535", n.minPort, n.maxPort)
	}
	return nil
}

// defaultSidecarListen picks where sidecars reach the agent.
//
// On Linux Docker runs on this machine, so a unix socket in a directory
// that is mounted into each sidecar does it, and no firewall is in the way.
// On macOS Docker runs in a virtual machine: a socket cannot cross into it,
// but the machine forwards to the host's loopback.
func defaultSidecarListen(ctx context.Context, daemon *docker.Client, node string) (string, error) {
	if runtime.GOOS != "linux" {
		return "127.0.0.1:" + sidecarPort, nil
	}
	if dir := runtimeDir(); dir != "" {
		return "unix://" + filepath.Join(dir, node, "agent.sock"), nil
	}
	// Nowhere to keep a socket: fall back to the Docker bridge's gateway,
	// which the host's firewall must then let containers reach.
	gateway, err := daemon.BridgeGateway(ctx)
	if err != nil {
		return "", err
	}
	return gateway + ":" + sidecarPort, nil
}

// runtimeDir is where this user may keep a socket: /run for root, or the
// directory the login session provides. Empty when there is neither.
func runtimeDir() string {
	if os.Geteuid() == 0 {
		return "/run/agones-lite"
	}
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "agones-lite")
	}
	return ""
}

// runAgent runs game servers on this machine for a control plane on another.
func runAgent(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	server := fs.String("server", "", "the control plane's --node-listen address, as host:port")
	token := fs.String("token", os.Getenv("AGONES_LITE_JOIN_TOKEN"), "join token (or $AGONES_LITE_JOIN_TOKEN)")
	tokenFile := fs.String("token-file", "", "file that holds the join token")
	var node nodeOptions
	node.register(fs)
	if err := parseWithConfig(fs, args); err != nil {
		return err
	}
	node.finish()
	joinToken, err := readToken(*token, *tokenFile)
	if err != nil || *server == "" {
		return errors.Join(err, errors.New("usage: agones-lite agent --server HOST:PORT --token TOKEN"))
	}
	if err := agones.Setup(); err != nil {
		return err
	}
	daemon, err := node.connect(ctx)
	if err != nil {
		return err
	}
	remote, err := nodes.Dial(*server, joinToken)
	if err != nil {
		return err
	}
	defer func() { _ = remote.Close() }()

	_, _ = fmt.Fprintf(out, "agones-lite: node %q joining %s, sidecars on %s\n", node.agent.NodeName, *server, node.agent.SidecarListen)
	local := agent.New(node.agent, remote, daemon)
	remote.Serve(local) // the control plane reads this node's logs through the link
	return runAll(ctx,
		func(ctx context.Context) error { remote.Run(ctx, node.agent.NodeName); return nil },
		local.Run,
	)
}

// readToken takes the token from the flag or, failing that, from a file.
func readToken(token, file string) (string, error) {
	if token != "" || file == "" {
		return token, nil
	}
	raw, err := os.ReadFile(file)
	return strings.TrimSpace(string(raw)), err
}
