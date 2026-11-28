package cli

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"

	"github.com/ponyo877/scrim/pkg/agent"
	"github.com/ponyo877/scrim/pkg/agones"
	scrimv1 "github.com/ponyo877/scrim/pkg/apis/scrim/v1"
	"github.com/ponyo877/scrim/pkg/docker"
	"github.com/ponyo877/scrim/pkg/nodes"
	"github.com/ponyo877/scrim/pkg/version"
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
	ranges       portRanges
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
	fs.Var(&n.ranges, "additional-port-ranges", `more ranges of host ports, by name, that a port of a game server can name with "range": {"game": [5000, 6000]}, as upstream's controller takes them`)
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

// finish copies the parsed numbers into the agent's configuration. They
// are cut to the size the configuration has for them; check refuses the
// ones that do not fit before anything is done with them.
func (n *nodeOptions) finish() {
	n.agent.PortRange = scrimv1.PortRange{Min: int32(n.minPort), Max: int32(n.maxPort)} //nolint:gosec // see above
	n.agent.MaxGameServers = int32(n.maxServers)                                        //nolint:gosec // see above
	n.agent.PortRanges = n.ranges
}

// connect checks the node's settings and that the Docker daemon answers,
// and fills in where the agent should listen for sidecars.
func (n *nodeOptions) connect(ctx context.Context) (*docker.Client, error) {
	if err := n.check(); err != nil {
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

// check refuses a port range that holds no port, and a limit that is no
// number of game servers. A node with such a range would register fine and
// then have no port for any game server; a limit of five thousand million
// would be cut to a number that means something else.
func (n *nodeOptions) check() error {
	if n.minPort < 1 || n.maxPort > 65535 || n.minPort > n.maxPort {
		return fmt.Errorf("--min-port %d and --max-port %d are not a port range: need 1 <= min <= max <= 65535", n.minPort, n.maxPort)
	}
	if n.maxServers < 0 || n.maxServers > math.MaxInt32 {
		return fmt.Errorf("--max-gameservers %d is not a number of game servers: need 0 <= n <= %d", n.maxServers, math.MaxInt32)
	}
	return apart(n.agent.PortRanges, n.agent.PortRange)
}

// portRanges is the value of --additional-port-ranges: named ranges of
// host ports, in the JSON that upstream's controller takes.
type portRanges map[string]scrimv1.PortRange

func (r *portRanges) String() string {
	if r == nil || len(*r) == 0 {
		return ""
	}
	var each []string
	for _, name := range slices.Sorted(maps.Keys(*r)) {
		each = append(each, fmt.Sprintf("%q: [%d, %d]", name, (*r)[name].Min, (*r)[name].Max))
	}
	return "{" + strings.Join(each, ", ") + "}"
}

// Set reads {"name": [min, max], ...}.
func (r *portRanges) Set(text string) error {
	var given map[string][]int
	if err := json.Unmarshal([]byte(text), &given); err != nil {
		return errors.New(`need a JSON object of named ranges: {"game": [5000, 6000]}`)
	}
	ranges := portRanges{}
	for name, bounds := range given {
		if name == "" || name == agonesv1.DefaultPortRange {
			return fmt.Errorf("%q: the default range is --min-port and --max-port", name)
		}
		if len(bounds) != 2 || bounds[0] < 1 || bounds[1] > 65535 || bounds[0] > bounds[1] {
			return fmt.Errorf("%s: %v is not a port range: need [min, max] with 1 <= min <= max <= 65535", name, bounds)
		}
		ranges[name] = scrimv1.PortRange{Min: int32(bounds[0]), Max: int32(bounds[1])} //nolint:gosec // checked above
	}
	*r = ranges
	return nil
}

// takesJSONObject lets a config file give the ranges as a map.
func (*portRanges) takesJSONObject() {}

// apart refuses ranges of a node that share a port. The allocator picks
// from one range after the other, counting what one gave as taken in the
// next; with ranges apart, the order never matters, and a GameServer that
// fits is never turned away.
func apart(named map[string]scrimv1.PortRange, first scrimv1.PortRange) error {
	ranges := maps.Clone(named)
	if ranges == nil {
		ranges = map[string]scrimv1.PortRange{}
	}
	ranges[agonesv1.DefaultPortRange] = first
	names := slices.SortedFunc(maps.Keys(ranges), func(a, b string) int {
		return cmp.Or(cmp.Compare(ranges[a].Min, ranges[b].Min), cmp.Compare(a, b))
	})
	for i := 1; i < len(names); i++ {
		low, high := ranges[names[i-1]], ranges[names[i]]
		if high.Min <= low.Max {
			return fmt.Errorf("the port ranges %s (%d-%d) and %s (%d-%d) overlap: the ranges of a node must not share a port",
				names[i-1], low.Min, low.Max, names[i], high.Min, high.Max)
		}
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
		return "/run/scrim"
	}
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "scrim")
	}
	return ""
}

// runAgent runs game servers on this machine for a control plane on another.
func runAgent(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	server := fs.String("server", "", "the control plane's --node-listen address, as host:port")
	token := fs.String("token", os.Getenv("SCRIM_JOIN_TOKEN"), "join token (or $SCRIM_JOIN_TOKEN)")
	tokenFile := fs.String("token-file", "", "file that holds the join token")
	var node nodeOptions
	node.register(fs)
	if err := parseWithConfig(fs, args); err != nil {
		return err
	}
	node.finish()
	joinToken, err := readToken(*token, *tokenFile)
	if err != nil || *server == "" {
		return errors.Join(err, errors.New("usage: scrim agent --server HOST:PORT --token TOKEN"))
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

	_, _ = fmt.Fprintf(out, "scrim: node %q joining %s, sidecars on %s\n", node.agent.NodeName, *server, node.agent.SidecarListen)
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
