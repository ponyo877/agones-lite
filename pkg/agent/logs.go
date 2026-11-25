package agent

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/ponyo877/scrim/pkg/docker"
	"github.com/ponyo877/scrim/pkg/podspec"
)

// LogRequest says which log to read.
type LogRequest struct {
	Namespace string
	Name      string
	// Container names one container of the GameServer. Empty means the game
	// server's main container.
	Container string
	// Follow keeps the stream open for new output.
	Follow bool
	// Tail limits the history to that many lines. Zero means all of it.
	Tail int
}

// ErrNoLog says that there is no log to read right now: the containers are
// not there yet, or not any more, or their node cannot be reached. That is
// nobody's fault, and asking again later may find one.
var ErrNoLog = errors.New("there is no log to read")

// Logs returns the output of one container of a GameServer on this node.
func (a *Agent) Logs(ctx context.Context, req LogRequest) (io.ReadCloser, error) {
	containers, err := a.docker.List(ctx, podspec.LabelNode+"="+a.cfg.NodeName)
	if err != nil {
		return nil, err
	}
	target, ok := logTarget(containers, req)
	if !ok {
		return nil, fmt.Errorf("gameserver %s/%s has no such container on node %s: %w", req.Namespace, req.Name, a.cfg.NodeName, ErrNoLog)
	}
	return a.docker.Logs(ctx, target.ID, req.Follow, req.Tail)
}

// logTarget finds the container a request is about.
func logTarget(containers []docker.Container, req LogRequest) (docker.Container, bool) {
	wanted := podspec.Prefix(req.Namespace, req.Name) + req.Container
	for _, c := range containers {
		if c.Labels[podspec.LabelNamespace] != req.Namespace || c.Labels[podspec.LabelGameServer] != req.Name {
			continue
		}
		main := req.Container == "" && c.Labels[podspec.LabelRole] == podspec.RoleGame
		if main || c.Name == wanted {
			return c, true
		}
	}
	return docker.Container{}, false
}
