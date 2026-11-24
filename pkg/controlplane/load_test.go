package controlplane

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ponyo877/agones-lite/pkg/agent"
	"github.com/ponyo877/agones-lite/pkg/agones"
	litev1 "github.com/ponyo877/agones-lite/pkg/apis/lite/v1"
	"github.com/ponyo877/agones-lite/pkg/events"
	"github.com/ponyo877/agones-lite/pkg/metrics"
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
// It takes about half a minute, so it only runs when asked:
//
//	AGONES_LITE_LOAD=1 go test ./pkg/controlplane/ -run TestLoad -v
func TestLoad(t *testing.T) {
	if os.Getenv("AGONES_LITE_LOAD") == "" {
		t.Skip("set AGONES_LITE_LOAD=1 to run the load test")
	}
	w, stop := newLoadWorld(t, loadNodes)
	defer stop()

	started := time.Now()
	w.resize(loadGameServers)
	w.waitFor(agonesv1.GameServerStateReady, loadGameServers)
	t.Logf("%d game servers on %d nodes were Ready after %s", loadGameServers, loadNodes, time.Since(started).Round(time.Millisecond))

	const seconds = 10
	took := w.allocateAtRate(loadAllocations, seconds)
	slices.Sort(took)
	median, p99, slowest := took[len(took)/2], took[len(took)*99/100], took[len(took)-1]
	t.Logf("%d allocations at %d per second: median %s, 99th percentile %s, slowest %s",
		len(took), loadAllocations, median.Round(time.Microsecond), p99.Round(time.Microsecond), slowest.Round(time.Microsecond))

	require.Len(t, took, loadAllocations*seconds, "every allocation succeeded")
	require.Less(t, p99, 100*time.Millisecond, "an allocation should not take a noticeable part of a second")
	require.Equal(t, loadAllocations*seconds, w.count(agonesv1.GameServerStateAllocated))
}

// BenchmarkScaleUp measures a new Fleet from the moment it is applied to
// the moment every game server is Ready: how fast the control plane hands
// out work, with nodes that take no time over theirs.
//
//	go test ./pkg/controlplane/ -run '^$' -bench ScaleUp -benchtime 3x
func BenchmarkScaleUp(b *testing.B) {
	for _, size := range []int{1000, 10000} {
		b.Run(fmt.Sprintf("%d-gameservers", size), func(b *testing.B) {
			for range b.N {
				b.StopTimer()
				w, stop := newLoadWorld(b, nodesFor(size))
				b.StartTimer()
				w.resize(size)
				w.waitFor(agonesv1.GameServerStateReady, size)
				b.StopTimer()
				stop()
			}
			b.ReportMetric(float64(size*b.N)/b.Elapsed().Seconds(), "gameservers/s")
		})
	}
}

// BenchmarkAllocate measures allocation as fast as it goes: a number of
// matchmakers ask without a pause, each waiting for its answer before it
// asks again. The Fleet is topped up in between, outside the measurement,
// so that the requests always choose among about as many Ready game
// servers as the name says.
//
//	go test ./pkg/controlplane/ -run '^$' -bench Allocate -benchtime 2000x
func BenchmarkAllocate(b *testing.B) {
	for _, ready := range []int{1000, 10000} {
		for _, clients := range []int{1, 64} {
			b.Run(fmt.Sprintf("among-%d/%d-clients", ready, clients), func(b *testing.B) {
				benchmarkAllocate(b, ready, clients)
			})
		}
	}
}

func benchmarkAllocate(b *testing.B, ready, clients int) {
	w, stop := newLoadWorld(b, nodesFor(ready+b.N))
	defer stop()
	var took []time.Duration
	b.ResetTimer()
	for done := 0; done < b.N; {
		b.StopTimer()
		round := min(ready/2, b.N-done)
		w.resize(done + ready) // those allocated so far stay, and count
		w.waitFor(agonesv1.GameServerStateReady, ready)
		b.StartTimer()
		took = append(took, w.allocateWith(clients, round)...)
		done += round
	}
	b.StopTimer()
	slices.Sort(took)
	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "allocations/s")
	b.ReportMetric(took[len(took)/2].Seconds()*1000, "median-ms")
	b.ReportMetric(took[len(took)*99/100].Seconds()*1000, "p99-ms")
}

// nodesFor is how many simulated nodes hold that many game servers, with
// room to spare: each has a thousand ports.
func nodesFor(gameServers int) int {
	return max(loadNodes, gameServers/500)
}

// newLoadWorld is the control plane as "agones-lite server" runs it, with
// the event history and the metrics on, and simulated nodes in place of
// agents. stop ends it.
func newLoadWorld(t testing.TB, nodeCount int) (w world, stop func()) {
	t.Helper()
	require.NoError(t, agones.Setup())
	database := filepath.Join(t.TempDir(), "state.db")
	s, err := store.Open(database, store.Kinds())
	require.NoError(t, err)
	recorder, err := events.Open(database)
	require.NoError(t, err)
	cp, err := New(s, recorder, metrics.New(s), nodes.DefaultGraces)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	var running sync.WaitGroup
	running.Go(func() { s.Run(ctx) })
	running.Go(func() { recorder.Run(ctx) })
	running.Go(func() { cp.Run(ctx) })
	for i := range nodeCount {
		node := &litev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("node-%03d", i)},
			Status: litev1.NodeStatus{
				Phase:     litev1.NodeReady,
				Address:   fmt.Sprintf("203.0.113.%d", i%250+1),
				PortRange: litev1.PortRange{Min: 7000, Max: 8000},
			},
		}
		running.Go(func() { _ = agent.NewSimulator(node, cp.Agents).Run(ctx) })
	}
	return world{t: t, cp: cp, api: cp.API.Handler(), store: s}, func() {
		cancel()
		running.Wait()
		_ = recorder.Close()
		_ = s.Close()
	}
}

// resize applies the Fleet with that many replicas.
func (w world) resize(replicas int) {
	w.t.Helper()
	manifest := strings.Replace(fleetManifest, "replicas: 3", fmt.Sprintf("replicas: %d", replicas), 1)
	w.call(http.MethodPut, "/v1/namespaces/default/fleets/simple", manifest)
}

// count is how many game servers are in a state. It looks without copying:
// with ten thousand of them, a copy for every look would be measured too.
func (w world) count(state agonesv1.GameServerState) int {
	n := 0
	for _, gs := range store.GameServersOf(w.store).View("default") {
		if gs.Status.State == state {
			n++
		}
	}
	return n
}

// waitFor returns when that many game servers are in a state.
func (w world) waitFor(state agonesv1.GameServerState, n int) {
	w.t.Helper()
	require.Eventually(w.t, func() bool { return w.count(state) == n }, 10*time.Minute, 2*time.Millisecond,
		"%d game servers become %s", n, state)
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

// allocateWith has that many clients make n allocations between them, one
// after the other each, and returns how long every request took.
func (w world) allocateWith(clients, n int) []time.Duration {
	took := make([]time.Duration, n)
	var next atomic.Int64
	var asking sync.WaitGroup
	for range clients {
		asking.Go(func() {
			for i := int(next.Add(1)) - 1; i < n; i = int(next.Add(1)) - 1 {
				started := time.Now()
				answer, err := w.allocate()
				took[i] = time.Since(started)
				if err != nil || answer.GetGameServerName() == "" {
					w.t.Errorf("allocation %d: %v, %v", i, answer, err)
				}
			}
		})
	}
	asking.Wait()
	return took
}
