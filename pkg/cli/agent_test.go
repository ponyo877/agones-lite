package cli

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeDocker answers every request with what the Docker daemon says about
// its API versions and about its bridge network, which is all that starting
// a node asks of it. It returns the socket it listens on.
func fakeDocker(t *testing.T) (host string) {
	t.Helper()
	// Not t.TempDir(): its path is too long for a unix socket on macOS.
	dir, err := os.MkdirTemp("", "docker")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	listener, err := net.Listen("unix", filepath.Join(dir, "docker.sock"))
	require.NoError(t, err)

	daemon := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ApiVersion": "1.52", "MinAPIVersion": "1.44", "IPAM": {"Config": [{"Gateway": "172.17.0.1"}]}}`))
	}))
	_ = daemon.Listener.Close()
	daemon.Listener = listener
	daemon.Start()
	t.Cleanup(daemon.Close)
	return "unix://" + listener.Addr().String()
}

func TestAPortRangeWithoutPortsIsRefusedAtStartup(t *testing.T) {
	tests := map[string][2]int{
		"the last port before the first": {8000, 7000},
		"a first port that is no port":   {0, 7000},
		"a last port that is no port":    {7000, 70000},
	}
	for name, ports := range tests {
		t.Run(name, func(t *testing.T) {
			n := nodeOptions{dockerHost: fakeDocker(t), minPort: ports[0], maxPort: ports[1]}

			_, err := n.connect(context.Background())

			assert.ErrorContains(t, err, "--min-port")
		})
	}

	one := nodeOptions{dockerHost: fakeDocker(t), minPort: 7000, maxPort: 7000}
	_, err := one.connect(context.Background())
	assert.NoError(t, err, "a range of one port is a range")
}

func TestTheDockerConfigIsLookedForWhereDockerKeepsIt(t *testing.T) {
	t.Setenv("DOCKER_CONFIG", "/etc/docker-credentials")
	o, err := serverFlags(nil)
	require.NoError(t, err)
	assert.Equal(t, "/etc/docker-credentials/config.json", o.node.dockerConfig)

	t.Setenv("DOCKER_CONFIG", "")
	t.Setenv("HOME", "/home/games")
	o, err = serverFlags(nil)
	require.NoError(t, err)
	assert.Equal(t, "/home/games/.docker/config.json", o.node.dockerConfig)

	o, err = serverFlags([]string{"--docker-config", "/run/secrets/registry.json"})
	require.NoError(t, err)
	assert.Equal(t, "/run/secrets/registry.json", o.node.dockerConfig)
}

func TestTheDockerClientGetsTheDockerConfig(t *testing.T) {
	n := nodeOptions{dockerHost: fakeDocker(t), dockerConfig: "/run/secrets/registry.json", minPort: 7000, maxPort: 8000}

	daemon, err := n.connect(context.Background())

	require.NoError(t, err)
	assert.Equal(t, "/run/secrets/registry.json", daemon.ConfigFile)
}
