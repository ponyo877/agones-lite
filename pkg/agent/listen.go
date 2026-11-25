package agent

import (
	"net"
	"os"
	"path/filepath"
	"strings"
)

// socketMount is where the directory of the agent's socket appears inside a
// sidecar container.
const socketMount = "/run/scrim"

// socketScheme marks a SidecarListen address that is a unix socket.
const socketScheme = "unix://"

// listen opens where sidecars reach the agent: "host:port" for TCP, or
// "unix:///some/dir/agent.sock" for a unix socket.
//
// A unix socket is the better choice wherever Docker runs on the same
// machine as the agent. TCP has to cross the host's firewall, and a host
// that refuses what it was not told to accept (the default of many cloud
// images, and of ufw) silently keeps every sidecar from its agent.
func listen(address string) (net.Listener, error) {
	path, isSocket := strings.CutPrefix(address, socketScheme)
	if !isSocket {
		return net.Listen("tcp", address)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	// An agent that was killed leaves its socket behind, and a socket that
	// exists cannot be listened on.
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	return net.Listen("unix", path)
}

// sidecarAccess says how a sidecar reaches the agent from inside its
// container: the address to dial, and what has to be mounted for it.
//
// Over TCP that is the configured address. A unix socket is reached through
// a mount of its directory. Mounting the socket itself would hand sidecars
// a dead one once the agent restarts and makes a new socket.
func (c Config) sidecarAccess() (address string, binds []string) {
	path, isSocket := strings.CutPrefix(c.SidecarListen, socketScheme)
	if !isSocket {
		return c.SidecarDial, nil
	}
	address = socketScheme + socketMount + "/" + filepath.Base(path)
	return address, []string{filepath.Dir(path) + ":" + socketMount}
}
