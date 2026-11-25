package controlplane

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
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
	"github.com/ponyo877/agones-lite/pkg/gameservers"
	"github.com/ponyo877/agones-lite/pkg/metrics"
	"github.com/ponyo877/agones-lite/pkg/nodes"
	"github.com/ponyo877/agones-lite/pkg/store"
)

// The scale agones-lite is designed for; see docs/planning.md.
const (
	loadNodes       = 10
	loadGameServers = 1000
	loadAllocations = 50 // per second
	// loadSession is how long a match lasts in TestLoad. Half the game
	// servers are in a match at any time, and fifty a second end and are
	// replaced while fifty more are asked for.
	loadSession = 10 * time.Second
)

// TestLoad checks the design targets with simulated nodes: the control
// plane, its controllers and SQLite are real, the containers are not.
//
// It takes a little over a minute, so it only runs when asked:
//
//	AGONES_LITE_LOAD=1 go test ./pkg/controlplane/ -run TestLoad -v
//
// How it measures follows what is known about measuring a tail. Requests
// come whether or not the ones before them are answered, at moments drawn
// beforehand as from independent callers, and a request's time counts from
// the moment it was due: a generator that waits for answers, or that counts
// from when it got round to sending, hides the very waiting it is after
// (Schroeder, Wierman and Harchol-Balter, "Open Versus Closed: A
// Cautionary Tale", NSDI 2006). And the 99th percentile of a sample is not
// that of what was sampled: see atMost.
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

	const seconds = 60 // 3,000 requests: fewer say little about one in a hundred
	took := w.allocateAtRate(loadAllocations, seconds)
	slices.Sort(took)
	median, p99, slowest := took[len(took)/2], took[len(took)*99/100], took[len(took)-1]
	t.Logf("%d allocations at %d per second: median %s, 99th percentile %s (at most %s, with 95%% confidence), slowest %s",
		len(took), loadAllocations, median.Round(time.Microsecond), p99.Round(time.Microsecond),
		atMost(took, 0.99).Round(time.Microsecond), slowest.Round(time.Microsecond))

	require.Len(t, took, loadAllocations*seconds, "every allocation got a game server, and one of its own")
	require.Less(t, atMost(took, 0.99), 100*time.Millisecond, "an allocation should not take a noticeable part of a second")
}

// atMost returns a time that the q-quantile of all allocations, and not
// just of the ones sampled, lies under with 95% confidence. sorted is the
// sample, smallest first.
//
// How many of n independent samples fall under the true quantile is
// binomial, so the quantile lies between two samples whose ranks are a
// little to either side of n*q: the interval Lancet uses to decide how
// long to measure (Kogias, Mallon and Bugnion, USENIX ATC 2019). This is
// its upper end. With 500 samples the upper end for the 99th percentile is
// the slowest sample of all, and with 3,000 it is the 19th slowest.
func atMost(sorted []time.Duration, q float64) time.Duration {
	n := float64(len(sorted))
	rank := int(math.Ceil(n*q+1.96*math.Sqrt(n*q*(1-q)))) + 1
	return sorted[min(rank, len(sorted))-1]
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
// The latencies are those of a closed loop: a slow answer keeps its
// matchmaker from asking, and is counted once. They say how long a request
// takes when the control plane is kept as busy as it can be, and not what
// requests arriving at that rate on their own would see, which is more.
// TestLoad measures that.
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

// count is how many game servers are in a state. The store keeps count:
// with ten thousand of them, a look at each for every time this is asked
// would be measured too.
func (w world) count(state agonesv1.GameServerState) int {
	byState := store.By[*agonesv1.GameServer, agonesv1.GameServerState]{
		Name: "state",
		Of:   func(gs *agonesv1.GameServer) agonesv1.GameServerState { return gs.Status.State },
	}
	return store.GroupSizes(store.GameServersOf(w.store), byState)[state]
}

// waitFor returns when that many game servers are in a state.
func (w world) waitFor(state agonesv1.GameServerState, n int) {
	w.t.Helper()
	require.Eventually(w.t, func() bool { return w.count(state) == n }, 10*time.Minute, 2*time.Millisecond,
		"%d game servers become %s", n, state)
}

// allocateAtRate asks for perSecond game servers a second on average, at
// the moments arrivals has drawn, and returns how long each request took
// that was answered with a game server nobody else had been given, from the
// moment it was due. A game server that is allocated plays its match and
// shuts down, as the game does through the SDK.
func (w world) allocateAtRate(perSecond, seconds int) []time.Duration {
	plan := arrivals(perSecond, seconds)
	results := make(chan time.Duration, len(plan))
	running, stop := context.WithCancel(context.Background())
	defer stop()
	// The game servers that are in a match, by name. A name is free again
	// once its match is over: with a few thousand game servers coming and
	// going, a new one now and then gets the name of one that is gone.
	var inAMatch sync.Map
	began := time.Now()
	for _, after := range plan {
		due := began.Add(after)
		time.Sleep(time.Until(due))
		go func() {
			answer, err := w.allocate()
			name := answer.GetGameServerName()
			if _, twice := inAMatch.LoadOrStore(name, true); err != nil || twice {
				w.t.Errorf("an allocation got %q, %v (in a match already: %t)", name, err, twice)
				results <- -1
				return
			}
			results <- time.Since(due)
			w.play(running, name)
			inAMatch.Delete(name)
		}()
	}
	var took []time.Duration
	for range plan {
		if d := <-results; d >= 0 {
			took = append(took, d)
		}
	}
	return took
}

// play is the match on a game server: it lasts loadSession, and then the
// game server says it is done.
func (w world) play(ctx context.Context, name string) {
	select {
	case <-ctx.Done():
	case <-time.After(loadSession):
		_ = w.cp.Agents.Report(ctx, gameservers.Report{Namespace: "default", Name: name, Event: gameservers.SDKShutdown})
	}
}

// arrivals draws the moments to ask at: perSecond*seconds of them, with
// pauses as between the requests of many callers who know nothing of each
// other (a Poisson process), and the same ones on every run. Evenly spaced
// requests would be the kindest there are to whatever queues inside.
func arrivals(perSecond, seconds int) []time.Duration {
	random := rand.New(rand.NewPCG(1, 2))
	plan := make([]time.Duration, perSecond*seconds)
	at := 0.0
	for i := range plan {
		at += random.ExpFloat64() / float64(perSecond)
		plan[i] = time.Duration(at * float64(time.Second))
	}
	return plan
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
