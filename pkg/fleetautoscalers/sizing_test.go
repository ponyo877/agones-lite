package fleetautoscalers

import (
	"math"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"pgregory.net/rapid"
)

func TestBufferFor(t *testing.T) {
	tests := map[string]struct {
		perSecond     float64
		startup, sync time.Duration
		miss          float64
		want          int
	}{
		"nobody asks":                         {0, 20 * time.Second, 30 * time.Second, 0.01, 0},
		"one request in the stretch":          {0.02, 20 * time.Second, 30 * time.Second, 0.01, 4},
		"ten":                                 {0.2, 20 * time.Second, 30 * time.Second, 0.01, 18},
		"a hundred":                           {2, 20 * time.Second, 30 * time.Second, 0.01, 124},
		"a thousand":                          {20, 20 * time.Second, 30 * time.Second, 0.01, 1074},
		"a quick start does not help much":    {2, time.Second, 30 * time.Second, 0.01, 81},
		"looking more often does":             {2, time.Second, 5 * time.Second, 0.01, 21},
		"one miss in a thousand costs a few":  {2, 20 * time.Second, 30 * time.Second, 0.001, 132},
		"one in twenty saves a few":           {2, 20 * time.Second, 30 * time.Second, 0.05, 117},
		"a mean too large to exponentiate":    {100, 500 * time.Second, 500 * time.Second, 0.01, 100736},
		"with every miss allowed, none kept":  {2, 20 * time.Second, 30 * time.Second, 1, 0},
		"a rate that is no rate, read as nil": {-1, 20 * time.Second, 30 * time.Second, 0.01, 0},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, test.want, BufferFor(test.perSecond, test.startup, test.sync, test.miss))
		})
	}
}

// The quantile is the smallest number of requests that is enough often
// enough: with it the chance of more is at most 1-p, and with one less it
// is not.
func TestPoissonQuantileIsTheSmallestThatIsEnough(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		mean := rapid.Float64Range(0.01, 400).Draw(t, "mean")
		p := rapid.Float64Range(0.5, 0.9999).Draw(t, "p")

		k := poissonQuantile(mean, p)

		atMost := func(k int) (sum float64) {
			for i := 0; i <= k; i++ {
				logFactorial, _ := math.Lgamma(float64(i) + 1)
				sum += math.Exp(float64(i)*math.Log(mean) - mean - logFactorial)
			}
			return sum
		}
		assert.GreaterOrEqual(t, atMost(k), p-1e-12)
		if k > 0 {
			assert.Less(t, atMost(k-1), p)
		}
	})
}

// What BufferFor promises, tried on a Fleet that is played out request by
// request: requests come at random, nobody's session ends, and at every
// look the autoscaler orders what is missing from the buffer, which is
// Ready a startup later. That is the case a buffer is for, a rush.
//
// Of the looks, the share after which a request found nothing before the
// next one must be no more than was asked for. And with nine tenths of the
// buffer it must be more: the buffer is not simply large. The second
// buffer tells apart "the sync and the startup together" from "the startup
// alone", which would be a buffer of fifteen.
func TestWithTheBufferOfBufferForARushRunsOutAsRarelyAsAskedFor(t *testing.T) {
	const perSecond, miss = 2.0, 0.05
	const startup, sync = 5 * time.Second, 30 * time.Second
	enough := BufferFor(perSecond, startup, sync, miss)

	assert.LessOrEqual(t, (&rush{buffer: enough}).play(perSecond, startup, sync), miss*1.2)
	assert.Greater(t, (&rush{buffer: enough * 9 / 10}).play(perSecond, startup, sync), miss*1.2)
}

// rush is a Fleet under a Buffer policy while requests only come.
type rush struct {
	buffer int
	ready  int
	// ordered is what the autoscaler has added and is not Ready yet: how
	// many, and from when on they are.
	ordered []order
}

type order struct {
	servers int
	readyAt float64
}

// play runs ten thousand looks of the autoscaler and returns the share of
// them that were followed by a request finding nothing Ready.
func (f *rush) play(perSecond float64, startup, sync time.Duration) float64 {
	random := rand.New(rand.NewPCG(7, 11))
	const looks = 10000
	f.ready = f.buffer
	missed, request := 0, random.ExpFloat64()/perSecond
	for look := range looks {
		now, next := float64(look)*sync.Seconds(), float64(look+1)*sync.Seconds()
		f.order(now + startup.Seconds())
		ranOut := false
		for ; request < next; request += random.ExpFloat64() / perSecond {
			ranOut = !f.take(request) || ranOut
		}
		if ranOut {
			missed++
		}
	}
	return float64(missed) / looks
}

// order is the autoscaler's look: with nobody's session ending, what is
// missing from the buffer is what was taken since the last look.
func (f *rush) order(readyAt float64) {
	coming := 0
	for _, o := range f.ordered {
		coming += o.servers
	}
	f.ordered = append(f.ordered, order{servers: f.buffer - f.ready - coming, readyAt: readyAt})
}

// take is one request at a moment: it gets a GameServer if one is Ready by
// then.
func (f *rush) take(at float64) bool {
	for len(f.ordered) > 0 && f.ordered[0].readyAt <= at {
		f.ready += f.ordered[0].servers
		f.ordered = f.ordered[1:]
	}
	if f.ready == 0 {
		return false
	}
	f.ready--
	return true
}
