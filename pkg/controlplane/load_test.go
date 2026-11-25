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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ponyo877/scrim/pkg/agent"
	"github.com/ponyo877/scrim/pkg/agones"
	scrimv1 "github.com/ponyo877/scrim/pkg/apis/scrim/v1"
	"github.com/ponyo877/scrim/pkg/events"
	"github.com/ponyo877/scrim/pkg/gameservers"
	"github.com/ponyo877/scrim/pkg/metrics"
	"github.com/ponyo877/scrim/pkg/nodes"
	"github.com/ponyo877/scrim/pkg/store"
)

// The scale scrim is designed for; see docs/planning.md.
const (
	loadNodes       = 10
	loadGameServers = 1000
	loadAllocations = 50 // per second
	// loadSession is how long a match lasts in TestLoad. Half the game
	// servers are in a match at any time, and fifty a second end and are
	// replaced while fifty more are asked for.
	loadSession = 10 * time.Second
	// loadTarget is what the 99th percentile of the allocations stays under:
	// an allocation should not take a noticeable part of a second.
	loadTarget = 100 * time.Millisecond
	// loadTries is how many times TestLoad measures at most: see there.
	loadTries = 3
	// loadSeconds is how long a measurement lasts. In that time 3,000
	// requests are made: fewer say little about one in a hundred.
	loadSeconds = 60
)

// TestLoad checks the design targets with simulated nodes: the control
// plane, its controllers and SQLite are real, the containers are not.
//
// It takes a little over a minute, so it only runs when asked:
//
//	SCRIM_LOAD=1 go test ./pkg/controlplane/ -run TestLoad -v
//
// How it measures follows what is known about measuring a tail. Requests
// come whether or not the ones before them are answered, at moments drawn
// beforehand as from independent callers, and a request's time counts from
// the moment it was due: a generator that waits for answers, or that counts
// from when it got round to sending, hides the very waiting it is after
// (Schroeder, Wierman and Harchol-Balter, "Open Versus Closed: A
// Cautionary Tale", NSDI 2006). And the 99th percentile of a sample is not
// that of what was sampled: see atMost.
//
// A slow answer has three places to come from, and the test says which:
// the disk (the slowest save), the machine the test is on (how long the
// test itself was kept from running), or the control plane, when it is
// neither of the two.
//
// An allocation waits for the save that is under way, and then for the one
// that takes its own change along. So a disk that stands still for half the
// target can make it miss the target, and so can a machine that does. When
// a measurement is over the target, the allocations that were under way
// during such a standstill are set aside. If the rest are over the target
// too, that is the control plane's own doing, and the test fails at once.
// If they are not, the measurement says nothing about the control plane,
// and is made again, loadTries times at most.
func TestLoad(t *testing.T) {
	if os.Getenv("SCRIM_LOAD") == "" {
		t.Skip("set SCRIM_LOAD=1 to run the load test")
	}
	for try := 1; ; try++ {
		tail, undisturbed := measureLoad(t, loadSeconds, nil)
		switch says(tail, undisturbed) {
		case holdsTheTarget:
			return
		case missesItByItself:
			t.Fatalf("an allocation should not take a noticeable part of a second: the 99th percentile lies under %s, and under %s for the allocations that neither the disk nor the machine held up", tail, undisturbed)
		}
		require.Less(t, try, loadTries,
			"the disk or the machine stood still in every measurement: they say nothing about the control plane, so measure where it is quieter")
		t.Log("the slow allocations were under way while the disk or the machine stood still: this says nothing about the control plane, and is measured again")
	}
}

// What a measurement says about the control plane.
const (
	holdsTheTarget   = "holds the target"
	missesItByItself = "misses the target by itself"
	saysNothing      = "says nothing"
)

// says judges a measurement by the two times measureLoad returns: the one
// for all allocations, and the one for those that met no standstill.
func says(tail, undisturbed time.Duration) string {
	switch {
	case tail < loadTarget:
		return holdsTheTarget
	case undisturbed >= loadTarget:
		return missesItByItself
	}
	return saysNothing
}

// TestLoadSetsAsideWhatADiskThatStoodStillHeldUp tries the judgement of
// TestLoad on a measurement whose outcome is known: a short one, with a disk
// that stands still for the first three tenths of every second. The
// allocations due then miss the target and the others do not, so the
// measurement has to say nothing about the control plane.
//
// It runs when TestLoad does, on the same machine: where that machine is
// too busy for the allocations the disk left alone, this fails as TestLoad
// would.
func TestLoadSetsAsideWhatADiskThatStoodStillHeldUp(t *testing.T) {
	if os.Getenv("SCRIM_LOAD") == "" {
		t.Skip("set SCRIM_LOAD=1 to run the load test")
	}
	began := time.Now()
	standsStill := func() error {
		if into := time.Since(began) % time.Second; into < 3*loadTarget {
			time.Sleep(3*loadTarget - into)
		}
		return nil
	}

	tail, undisturbed := measureLoad(t, 5, standsStill)

	assert.GreaterOrEqual(t, tail, loadTarget, "the disk held allocations up for longer than the target")
	assert.Equal(t, saysNothing, says(tail, undisturbed),
		"only the allocations that the disk held up were slow: %s for all, %s for the rest", tail, undisturbed)
}

func TestWhatAMeasurementSaysAboutTheControlPlane(t *testing.T) {
	const quick, slow = 3 * time.Millisecond, 230 * time.Millisecond
	tests := map[string]struct {
		tail, undisturbed time.Duration
		says              string
	}{
		"every allocation was quick":                         {tail: quick, undisturbed: quick, says: holdsTheTarget},
		"slow ones, with nothing standing still":             {tail: slow, undisturbed: slow, says: missesItByItself},
		"slow ones, and others than those of the standstill": {tail: 2 * slow, undisturbed: slow, says: missesItByItself},
		"slow ones, all of them during a standstill":         {tail: slow, undisturbed: quick, says: saysNothing},
		"nothing but standstills":                            {tail: slow, undisturbed: 0, says: saysNothing},
		"just under the target":                              {tail: loadTarget - 1, undisturbed: loadTarget - 1, says: holdsTheTarget},
		"just at the target":                                 {tail: loadTarget, undisturbed: loadTarget, says: missesItByItself},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, test.says, says(test.tail, test.undisturbed))
		})
	}
}

func TestAnAllocationMeetsTheStandstillItWasUnderWayDuring(t *testing.T) {
	seen := &outside{}
	seen.saved(loadTarget/2 - time.Millisecond) // too short to count
	seen.saved(300 * time.Millisecond)          // the disk stood still until now
	ended := time.Now()
	began := ended.Add(-300 * time.Millisecond)

	tests := map[string]struct {
		answer answer
		met    bool
	}{
		"answered before it began":       {answer{due: began.Add(-time.Second), took: 100 * time.Millisecond}, false},
		"waiting when it began":          {answer{due: began.Add(-10 * time.Millisecond), took: 400 * time.Millisecond}, true},
		"due in the middle of it":        {answer{due: began.Add(100 * time.Millisecond), took: 250 * time.Millisecond}, true},
		"due and answered in the middle": {answer{due: began.Add(100 * time.Millisecond), took: time.Millisecond}, true},
		"due after it ended":             {answer{due: ended.Add(time.Second), took: time.Millisecond}, false},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, test.met, seen.met(test.answer))
		})
	}
	save, hold := seen.longest()
	assert.Equal(t, 300*time.Millisecond, save)
	assert.Zero(t, hold)
}

// measureLoad makes one measurement of that many seconds, on a control
// plane of its own. tail is the time that the 99th percentile of the
// allocations lies under, and undisturbed is the same for those that met no
// standstill of the disk or of the machine. With a disk given, every save
// waits for it first: that is how a test makes the disk stand still.
func measureLoad(t *testing.T, seconds int, disk func() error) (tail, undisturbed time.Duration) {
	w, stop := newLoadWorld(t, loadNodes, disk)
	defer stop()

	started := time.Now()
	w.resize(loadGameServers)
	w.waitFor(agonesv1.GameServerStateReady, loadGameServers)
	t.Logf("%d game servers on %d nodes were Ready after %s", loadGameServers, loadNodes, time.Since(started).Round(time.Millisecond))

	w.seen.forget()
	stopWatching := w.seen.watch()
	answers := w.allocateAtRate(loadAllocations, seconds)
	stopWatching()
	require.Len(t, answers, loadAllocations*seconds, "every allocation got a game server, and one of its own")

	took := times(answers, func(answer) bool { return true })
	calm := times(answers, func(a answer) bool { return !w.seen.met(a) })
	median, p99, slowest := took[len(took)/2], took[len(took)*99/100], took[len(took)-1]
	tail = atMost(took, 0.99)
	t.Logf("%d allocations at %d per second: median %s, 99th percentile %s (at most %s, with 95%% confidence), slowest %s",
		len(took), loadAllocations, median.Round(time.Microsecond), p99.Round(time.Microsecond),
		tail.Round(time.Microsecond), slowest.Round(time.Microsecond))
	save, hold := w.seen.longest()
	t.Logf("meanwhile the slowest save took %s, and the test itself was kept from running for at most %s: %d allocations were under way during a standstill of %s or more",
		save.Round(time.Microsecond), hold.Round(time.Microsecond), len(took)-len(calm), loadTarget/2)
	return tail, atMost(calm, 0.99)
}

// answer is an allocation that was answered: the moment it was due, and how
// long it took from then.
type answer struct {
	due  time.Time
	took time.Duration
}

// times returns how long the answers took that keep says to count,
// shortest first.
func times(answers []answer, keep func(answer) bool) []time.Duration {
	var took []time.Duration
	for _, a := range answers {
		if keep(a) {
			took = append(took, a.took)
		}
	}
	slices.Sort(took)
	return took
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
// the slowest sample of all, and with 3,000 it is the 19th slowest. Of no
// samples there is nothing to say, and it says zero.
func atMost(sorted []time.Duration, q float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	n := float64(len(sorted))
	rank := int(math.Ceil(n*q+1.96*math.Sqrt(n*q*(1-q)))) + 1
	return sorted[min(rank, len(sorted))-1]
}

// outside is what happened outside the control plane while it was measured:
// the disk, in the saves, and the machine, in how long the test itself was
// kept from running. It keeps the longest of each, and every stretch in
// which one of them stood still for half the target or more.
type outside struct {
	mu          sync.Mutex
	slowestSave time.Duration
	longestHold time.Duration
	standstills []stretch
}

// stretch is a stretch of time.
type stretch struct{ from, to time.Time }

// saved is told how long a save took that has just ended.
func (o *outside) saved(took time.Duration) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.slowestSave = max(o.slowestSave, took)
	o.stoodStill(took)
}

// held is told how long the test was kept from running until just now.
func (o *outside) held(late time.Duration) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.longestHold = max(o.longestHold, late)
	o.stoodStill(late)
}

// stoodStill keeps the stretch that has just ended, if it was long enough
// to make an allocation miss the target. Called with the lock held.
func (o *outside) stoodStill(took time.Duration) {
	if took >= loadTarget/2 {
		now := time.Now()
		o.standstills = append(o.standstills, stretch{from: now.Add(-took), to: now})
	}
}

// forget starts over: what the disk did before the measurement began is
// not the measurement's business.
func (o *outside) forget() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.slowestSave, o.longestHold, o.standstills = 0, 0, nil
}

// longest returns how long the slowest save took, and the longest that the
// test was kept from running.
func (o *outside) longest() (save, hold time.Duration) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.slowestSave, o.longestHold
}

// met reports whether an allocation was under way during a standstill.
func (o *outside) met(a answer) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.ContainsFunc(o.standstills, func(s stretch) bool {
		return a.due.Before(s.to) && s.from.Before(a.due.Add(a.took))
	})
}

// watch notes how long the test itself is kept from running, until stop is
// called: it asks to be woken every millisecond, and how much later than
// that it was is how long it was held. A machine that stands still for a
// moment, as a shared one does now and then, shows here, and in every
// request due meanwhile.
func (o *outside) watch() (stop func()) {
	watching, stop := context.WithCancel(context.Background())
	go func() {
		for last := time.Now(); watching.Err() == nil; last = time.Now() {
			time.Sleep(time.Millisecond)
			o.held(time.Since(last) - time.Millisecond)
		}
	}()
	return stop
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
				w, stop := newLoadWorld(b, nodesFor(size), nil)
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
	w, stop := newLoadWorld(b, nodesFor(ready+b.N), nil)
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

// newLoadWorld is the control plane as "scrim server" runs it, with
// the event history and the metrics on, and simulated nodes in place of
// agents. stop ends it. disk, when there is one, is what every save waits
// for first.
func newLoadWorld(t testing.TB, nodeCount int, disk func() error) (w world, stop func()) {
	t.Helper()
	require.NoError(t, agones.Setup())
	database := filepath.Join(t.TempDir(), "state.db")
	s, err := store.Open(database, store.Kinds())
	require.NoError(t, err)
	s.Disk = disk
	recorder, err := events.Open(database)
	require.NoError(t, err)
	cp, err := New(s, recorder, metrics.New(s), nodes.DefaultGraces)
	require.NoError(t, err)
	seen, measure := &outside{}, s.Saved
	s.Saved = func(changes int, took time.Duration) {
		measure(changes, took)
		seen.saved(took)
	}

	ctx, cancel := context.WithCancel(context.Background())
	var running sync.WaitGroup
	running.Go(func() { s.Run(ctx) })
	running.Go(func() { recorder.Run(ctx) })
	running.Go(func() { cp.Run(ctx) })
	for i := range nodeCount {
		node := &scrimv1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("node-%03d", i)},
			Status: scrimv1.NodeStatus{
				Phase:     scrimv1.NodeReady,
				Address:   fmt.Sprintf("203.0.113.%d", i%250+1),
				PortRange: scrimv1.PortRange{Min: 7000, Max: 8000},
			},
		}
		running.Go(func() { _ = agent.NewSimulator(node, cp.Agents).Run(ctx) })
	}
	return world{t: t, cp: cp, api: cp.API.Handler(), store: s, seen: seen}, func() {
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
// the moments arrivals has drawn, and returns the requests that were
// answered with a game server nobody else had been given, each with how
// long it took from the moment it was due. A game server that is allocated
// plays its match and shuts down, as the game does through the SDK.
func (w world) allocateAtRate(perSecond, seconds int) []answer {
	plan := arrivals(perSecond, seconds)
	results := make(chan answer, len(plan))
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
			given, err := w.allocate()
			name := given.GetGameServerName()
			if _, twice := inAMatch.LoadOrStore(name, true); err != nil || twice {
				w.t.Errorf("an allocation got %q, %v (in a match already: %t)", name, err, twice)
				results <- answer{took: -1}
				return
			}
			results <- answer{due: due, took: time.Since(due)}
			w.play(running, name)
			inAMatch.Delete(name)
		}()
	}
	var answers []answer
	for range plan {
		if a := <-results; a.took >= 0 {
			answers = append(answers, a)
		}
	}
	return answers
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
