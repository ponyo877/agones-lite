package agent

import (
	"context"
	"log/slog"
	"maps"
	"slices"

	"github.com/ponyo877/scrim/pkg/docker"
	"github.com/ponyo877/scrim/pkg/podspec"
)

// warnOfUnmanaged says, once when the agent starts, that the Docker daemon
// has containers that carry the name of another node.
//
// An agent that is started again under another --node-name leaves the
// containers of the old name behind. They run on, managed by nobody, and
// hold their host ports, on which new game servers then fail to start.
// Nothing is removed here: two agents may share one daemon on purpose.
func (a *Agent) warnOfUnmanaged(ctx context.Context) {
	all, err := a.docker.List(ctx, podspec.LabelNode)
	if err != nil {
		return // the first pass says what is wrong with Docker
	}
	others, total := unmanaged(all, a.cfg.NodeName)
	if total == 0 {
		return
	}
	slog.Warn("this Docker daemon has game server containers of other nodes, which this agent leaves alone; "+
		"unless another agent runs here, they are left over from an earlier --node-name and hold their host ports",
		"containers", total, "nodes", others,
		"remedy", "for each node: scrim node delete <node>, then docker rm -f $(docker ps -aq --filter label="+podspec.LabelNode+"=<node>)")
}

// unmanaged names the other nodes that containers belong to, and counts
// those containers.
func unmanaged(all []docker.Container, node string) (others []string, total int) {
	named := map[string]bool{}
	for _, c := range all {
		if owner := c.Labels[podspec.LabelNode]; owner != "" && owner != node {
			named[owner] = true
			total++
		}
	}
	return slices.Sorted(maps.Keys(named)), total
}
