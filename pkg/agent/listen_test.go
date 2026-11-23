package agent

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// socketDir returns a directory with a path short enough for a socket,
// which t.TempDir() does not promise.
func socketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "al")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func TestListenOnAUnixSocketMakesItsDirectoryAndClearsAStaleSocket(t *testing.T) {
	path := filepath.Join(socketDir(t), "node-1", "agent.sock")

	first, err := listen("unix://" + path)
	require.NoError(t, err)
	// An agent that is killed has no chance to remove its socket.
	first.(*net.UnixListener).SetUnlinkOnClose(false)
	require.NoError(t, first.Close())
	require.FileExists(t, path)

	second, err := listen("unix://" + path)
	require.NoError(t, err, "a socket left behind must not keep the next agent from starting")
	defer func() { _ = second.Close() }()
	conn, err := net.Dial("unix", path)
	require.NoError(t, err)
	_ = conn.Close()
}

func TestListenOnTCPStillWorks(t *testing.T) {
	listener, err := listen("127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = listener.Close() }()
	assert.Equal(t, "tcp", listener.Addr().Network())
}

func TestASidecarReachesAUnixSocketThroughAMountOfItsDirectory(t *testing.T) {
	socket := Config{SidecarListen: "unix:///run/agones-lite/node-1/agent.sock", SidecarDial: "ignored:1"}
	address, binds := socket.sidecarAccess()
	assert.Equal(t, "unix:///run/agones-lite/agent.sock", address)
	assert.Equal(t, []string{"/run/agones-lite/node-1:/run/agones-lite"}, binds,
		"the directory, not the socket: a restarted agent makes a new socket in it")

	tcp := Config{SidecarListen: "172.17.0.1:9447", SidecarDial: "host.docker.internal:9447"}
	address, binds = tcp.sidecarAccess()
	assert.Equal(t, "host.docker.internal:9447", address)
	assert.Empty(t, binds)
}
