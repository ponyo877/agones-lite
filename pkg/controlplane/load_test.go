package controlplane

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ponyo877/agones-lite/pkg/agent"
	"github.com/ponyo877/agones-lite/pkg/agones"
	litev1 "github.com/ponyo877/agones-lite/pkg/apis/lite/v1"
	"github.com/ponyo877/agones-lite/pkg/nodes"
	"github.com/ponyo877/agones-lite/pkg/store"
)

// The scale agones-lite is designed for; see docs/planning.md.
const (
	loadNodes       = 10
	loadGameServers = 1000
	loadAllocations = 50 // per second
)

// TestLoad checks the design targets with simulated nodes: the control
// plane, its controllers and SQLite are real, the containers are not.
//
// It takes about a minute, so it only runs when asked:
//
//	AGONES_LITE_LOAD=1 go test ./pkg/controlplane/ -run TestLoad -v
func TestLoad(t *testing.T) {
	if os.Getenv("AGONES_LITE_LOAD") == "" {
		t.Skip("set AGONES_LITE_LOAD=1 to run the load test")
	}
	w := newLoadWorld(t)

	started := time.Now()
	manifest := strings.Replace(fleetManifest, "replicas: 3", fmt.Sprintf("replicas: %d", loadGameServers), 1)
	w.call(http.MethodPut, "/v1/namespaces/default/fleets/simple", manifest)
	require.Eventually(t, func() bool {
		return w.states()[agonesv1.GameServerStateReady] == loadGameServers
	}, 5*time.Minute, 200*time.Millisecond, "every game server becomes Ready")
	t.Logf("%d game servers on %d nodes were Ready after %s", loadGameServers, loadNodes, time.Since(started).Round(time.Millisecond))

	const seconds = 10
	took := w.allocateAtRate(loadAllocations, seconds)
	sort.Slice(took, func(i, j int) bool { return took[i] < took[j] })
	median, p99, slowest := took[len(took)/2], took[len(took)*99/100], took[len(took)-1]
	t.Logf("%d allocations at %d per second: median %s, 99th percentile %s, slowest %s",
		len(took), loadAllocations, median.Round(time.Microsecond), p99.Round(time.Microsecond), slowest.Round(time.Microsecond))

	require.Len(t, took, loadAllocations*seconds, "every allocation succeeded")
	require.Less(t, p99, 100*time.Millisecond, "an allocation should not take a noticeable part of a second")
	require.Equal(t, loadAllocations*seconds, w.states()[agonesv1.GameServerStateAllocated])
}

// newLoadWorld is newWorld with loadNodes simulated nodes instead of one.
func newLoadWorld(t *testing.T) world {
	t.Helper()
	require.NoError(t, agones.Setup())
	s, err := store.Open(filepath.Join(t.TempDir(), "state.db"), store.Kinds())
	require.NoError(t, err)
	cp, err := New(s, nil, nil, nodes.DefaultGraces)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		cp.Run(ctx)
	}()
	for i := range loadNodes {
		node := &litev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("node-%02d", i)},
			Status: litev1.NodeStatus{
				Phase:     litev1.NodeReady,
				Address:   fmt.Sprintf("203.0.113.%d", 10+i),
				PortRange: litev1.PortRange{Min: 7000, Max: 8000},
			},
		}
		go func() { _ = agent.NewSimulator(node, cp.Agents).Run(ctx) }()
	}
	t.Cleanup(func() {
		cancel()
		<-done
		_ = s.Close()
	})
	return world{t: t, cp: cp, api: cp.API.Handler(), store: s}
}

// allocateAtRate asks for perSecond game servers every second, evenly
// spaced, and returns how long each successful request took.
func (w world) allocateAtRate(perSecond, seconds int) []time.Duration {
	results := make(chan time.Duration, perSecond*seconds)
	tick := time.NewTicker(time.Second / time.Duration(perSecond))
	defer tick.Stop()
	for range perSecond * seconds {
		<-tick.C
		go func() {
			started := time.Now()
			if _, err := w.allocate(); err != nil {
				results <- -1
				return
			}
			results <- time.Since(started)
		}()
	}
	var took []time.Duration
	for range perSecond * seconds {
		if d := <-results; d >= 0 {
			took = append(took, d)
		}
	}
	return took
}
