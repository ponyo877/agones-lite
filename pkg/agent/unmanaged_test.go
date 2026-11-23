package agent

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/ponyo877/agones-lite/pkg/docker"
	"github.com/ponyo877/agones-lite/pkg/podspec"
)

// logBook collects what is logged while a test runs.
type logBook struct {
	mu   sync.Mutex
	text strings.Builder
}

func (l *logBook) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.text.Write(p)
}

func (l *logBook) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.text.String()
}

func logged(t *testing.T) *logBook {
	t.Helper()
	book, before := &logBook{}, slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(book, nil)))
	t.Cleanup(func() { slog.SetDefault(before) })
	return book
}

func onNode(node string) docker.Container {
	c := container("gs", podspec.RoleGame, "running", 0)
	c.Labels[podspec.LabelNode] = node
	return c
}

// An agent started again under another --node-name leaves the containers of
// the old name running. Nobody manages them, and they hold their host ports.
func TestAnAgentSaysThatItsDaemonHasContainersOfOtherNodes(t *testing.T) {
	log := logged(t)
	d := &fakeDocker{containers: []docker.Container{onNode("node-1"), onNode("renamed"), onNode("renamed"), onNode("another")}}
	a, _ := newAgent(d)

	a.warnOfUnmanaged(context.Background())

	assert.Equal(t, 1, strings.Count(log.String(), "level=WARN"))
	assert.Contains(t, log.String(), "containers=3")
	assert.Contains(t, log.String(), `nodes="[another renamed]"`)
	assert.Contains(t, log.String(), "agones-lite node delete", "the remedy is part of the message")
	assert.Empty(t, d.calls, "nothing is removed: two agents may share one daemon on purpose")
}

func TestAnAgentAloneOnItsDaemonHasNothingToSay(t *testing.T) {
	log := logged(t)
	a, _ := newAgent(&fakeDocker{containers: []docker.Container{onNode("node-1"), onNode("node-1")}})

	a.warnOfUnmanaged(context.Background())

	assert.Empty(t, log.String())
}
