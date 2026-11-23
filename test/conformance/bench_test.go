package conformance

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	"github.com/stretchr/testify/require"

	"github.com/ponyo877/agones-lite/pkg/agent"
	"github.com/ponyo877/agones-lite/pkg/agones"
	litev1 "github.com/ponyo877/agones-lite/pkg/apis/lite/v1"
	"github.com/ponyo877/agones-lite/pkg/controlplane"
	"github.com/ponyo877/agones-lite/pkg/docker"
	"github.com/ponyo877/agones-lite/pkg/nodes"
	"github.com/ponyo877/agones-lite/pkg/podspec"
	"github.com/ponyo877/agones-lite/pkg/sdkserver"
	"github.com/ponyo877/agones-lite/pkg/store"
)

// manifest is a GameServer with a Counter and a List, a health check that
// is quick to fail, and its SDK server on two ports of the test's choosing.
const manifest = `
apiVersion: agones.dev/v1
kind: GameServer
metadata:
  name: %s
spec:
  sdkServer:
    grpcPort: %d
    httpPort: %d
  health:
    initialDelaySeconds: 1
    periodSeconds: 1
    failureThreshold: 2
  counters:
    rooms:
      count: 1
      capacity: 10
  lists:
    players:
      capacity: 4
      values: [ada]
  ports:
    - name: default
      containerPort: 7654
  template:
    spec:
      containers:
        - name: game
          image: example/game:1
`

// bench is one node with everything on it except Docker.
type bench struct {
	t       *testing.T
	api     http.Handler
	servers store.Typed[*agonesv1.GameServer]
}

// newBench is a node whose sidecars reach the agent through a unix socket,
// as they do on Linux.
func newBench(t *testing.T) bench {
	t.Helper()
	// Not t.TempDir(): a socket's path has to be short.
	dir, err := os.MkdirTemp("", "al")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return startBench(t, "unix://"+filepath.Join(dir, "agent.sock"), "")
}

// newBenchOverTCP is a node whose sidecars reach the agent over TCP, as
// they do where Docker runs in a virtual machine.
func newBenchOverTCP(t *testing.T) bench {
	t.Helper()
	address := "127.0.0.1:" + strconv.Itoa(freePort(t))
	return startBench(t, address, address)
}

func startBench(t *testing.T, listen, dial string) bench {
	t.Helper()
	require.NoError(t, agones.Setup())
	state, err := store.Open(filepath.Join(t.TempDir(), "state.db"), store.Kinds())
	require.NoError(t, err)
	cp, err := controlplane.New(state, nil, nil, nodes.DefaultGraces)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	containers := &containers{ctx: ctx, byID: map[string]*container{}}
	node := agent.New(agent.Config{
		NodeName:      "node-1",
		Address:       "203.0.113.10",
		PortRange:     litev1.PortRange{Min: 7000, Max: 7100},
		SidecarImage:  "sidecar:test",
		SidecarListen: listen,
		SidecarDial:   dial,
	}, cp.Agents, containers)

	var running sync.WaitGroup
	running.Go(func() { cp.Run(ctx) })
	running.Go(func() { _ = node.Run(ctx) })
	t.Cleanup(func() {
		cancel()
		running.Wait()
		_ = state.Close()
	})
	return bench{t: t, api: cp.API.Handler(), servers: store.GameServersOf(state)}
}

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = listener.Close() }()
	return listener.Addr().(*net.TCPAddr).Port
}

// create applies the manifest under name and waits until the containers
// "run", which is when a game server starts talking to the SDK. It returns
// the ports of the SDK server.
func (b bench) create(name string) (grpcPort, httpPort int) {
	b.t.Helper()
	grpcPort, httpPort = freePort(b.t), freePort(b.t)
	body := fmt.Sprintf(manifest, name, grpcPort, httpPort)
	rec := httptest.NewRecorder()
	b.api.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/namespaces/default/gameservers", strings.NewReader(body)))
	require.Equal(b.t, http.StatusCreated, rec.Code, rec.Body.String())
	b.becomes(name, agonesv1.GameServerStateScheduled)
	return grpcPort, httpPort
}

// stored returns the GameServer as the control plane has it, or nil.
func (b bench) stored(name string) *agonesv1.GameServer {
	gs, _ := b.servers.Get("default", name)
	return gs
}

func (b bench) soon(condition func() bool, message string, args ...any) {
	b.t.Helper()
	require.Eventuallyf(b.t, condition, 15*time.Second, 20*time.Millisecond, message, args...)
}

func (b bench) becomes(name string, state agonesv1.GameServerState) {
	b.t.Helper()
	b.soon(func() bool {
		gs := b.stored(name)
		return gs != nil && gs.Status.State == state
	}, "%s becomes %s", name, state)
}

// containers stands in for the Docker daemon.
type containers struct {
	ctx  context.Context // ends with the test
	mu   sync.Mutex
	byID map[string]*container
}

type container struct {
	docker.Container
	env   map[string]string
	binds []string           // "host directory:directory in the container"
	stop  context.CancelFunc // set while a sidecar's SDK server runs
}

func (c *containers) List(_ context.Context, label string) ([]docker.Container, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	key, value, _ := strings.Cut(label, "=")
	var out []docker.Container
	for _, held := range c.byID {
		if held.Labels[key] == value {
			out = append(out, held.Container)
		}
	}
	return out, nil
}

func (c *containers) Create(_ context.Context, spec docker.Spec) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := fmt.Sprintf("container-%d", len(c.byID)+1)
	env := map[string]string{}
	for _, pair := range spec.Env {
		key, value, _ := strings.Cut(pair, "=")
		env[key] = value
	}
	c.byID[id] = &container{
		Container: docker.Container{ID: id, Name: spec.Name, Labels: spec.Labels, State: "created"},
		env:       env,
		binds:     spec.Binds,
	}
	return id, nil
}

// Start marks the container as running. A sidecar really does run: it is
// the SDK server under test.
func (c *containers) Start(_ context.Context, id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	held := c.byID[id]
	held.State = "running"
	if held.Labels[podspec.LabelRole] != podspec.RoleSidecar {
		return nil
	}
	grpcPort, _ := strconv.Atoi(held.env[podspec.EnvGRPCPort])
	httpPort, _ := strconv.Atoi(held.env[podspec.EnvHTTPPort])
	creds := sdkserver.Credentials{GameServer: held.env[podspec.EnvGameServer], Token: held.env[podspec.EnvToken]}
	ctx, stop := context.WithCancel(c.ctx)
	held.stop = stop
	agentAddress := held.onHost(held.env[podspec.EnvAgent])
	go func() { _ = sdkserver.Serve(ctx, agentAddress, creds, grpcPort, httpPort) }()
	return nil
}

// onHost turns an address as the container sees it into the one this test
// can dial. A unix socket lies in a directory that was mounted into the
// container, so its path is followed back through the mount, which is what
// makes the test fail if the agent forgets to ask for the mount.
func (held *container) onHost(address string) string {
	path, isSocket := strings.CutPrefix(address, "unix://")
	if !isSocket {
		return address
	}
	for _, bind := range held.binds {
		host, inside, _ := strings.Cut(bind, ":")
		if rest, ok := strings.CutPrefix(path, inside); ok {
			return "unix://" + host + rest
		}
	}
	return address // not mounted: dialling this fails, as it would for real
}

func (c *containers) Stop(_ context.Context, id string, _ time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if held, ok := c.byID[id]; ok {
		held.halt()
	}
	return nil
}

func (c *containers) Remove(_ context.Context, id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if held, ok := c.byID[id]; ok {
		held.halt()
		delete(c.byID, id)
	}
	return nil
}

func (held *container) halt() {
	held.State = "exited"
	if held.stop != nil {
		held.stop()
	}
}

func (c *containers) HasImage(context.Context, string) (bool, error) { return true, nil }
func (c *containers) Pull(context.Context, string) error             { return nil }
func (c *containers) RemoveVolumes(context.Context, string) error    { return nil }
func (c *containers) Logs(context.Context, string, bool, int) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}
