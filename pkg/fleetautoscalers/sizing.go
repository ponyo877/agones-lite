package fleetautoscalers

import (
	"math"
	"time"
)

// BufferFor returns the bufferSize that keeps a Fleet from running out of
// Ready GameServers: the smallest one with which, of all the stretches of
// time the Fleet has to get by on what it has, no more than the share miss
// see a request go unanswered.
//
// How long is such a stretch? A Buffer policy tops the Fleet up when the
// autoscaler next looks, every sync, and what it adds is Ready a startup
// later. So the GameServers that are Ready or on their way at one look are
// all there is for the requests of the next sync and startup together, and
// not of the startup alone. With a GameServer that starts in five seconds
// and the autoscaler's usual thirty between looks, it is the thirty that
// sets the buffer.
//
// Requests from matchmakers that know nothing of each other come as a
// Poisson process, so how many come in such a stretch is Poisson with mean
// perSecond times its length, and the buffer is that distribution's
// quantile at 1-miss. For a large mean a this is about a + z*sqrt(a), the
// square-root rule of staffing (Green, Kolesar and Whitt, "Coping with
// Time-Varying Demand When Setting Staffing Requirements for a Service
// System", Production and Operations Management 16(1), 2007): the spare
// part grows with the root of the demand, which is why a percentage that
// is right for a large Fleet is too little for a small one.
//
// The answer is on the safe side. It counts no session that ends within the
// stretch and is replaced at once, and it is for a rate that holds; where
// the rate climbs, ask for the highest it reaches.
func BufferFor(perSecond float64, startup, sync time.Duration, miss float64) int {
	return poissonQuantile(perSecond*(startup+sync).Seconds(), 1-miss)
}

// poissonQuantile returns the smallest k with P(X <= k) >= p, for X Poisson
// with that mean: summed term by term, each from its logarithm, so that
// neither a mean of a hundred thousand nor a k in the hundreds overflows.
func poissonQuantile(mean, p float64) int {
	if mean <= 0 {
		return 0
	}
	// Further out than this there is nothing left to add that a float holds.
	last := int(mean + 40*math.Sqrt(mean) + 40)
	below := 0.0
	for k := range last {
		logFactorial, _ := math.Lgamma(float64(k) + 1)
		below += math.Exp(float64(k)*math.Log(mean) - mean - logFactorial)
		if below >= p {
			return k
		}
	}
	return last
}
