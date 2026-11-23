package sidecarv1

import (
	"math"
	"time"
)

// ReserveFor turns the seconds of a Reserve into a Duration. It lives beside
// the protocol because all who carry those seconds meet here: the sidecar,
// the agent and the control plane.
//
// More seconds than a Duration holds (292 years) would wrap around into a
// short or a negative time, so they count as the longest one. Upstream lets
// them wrap, to a time that is up at once. Zero or less means "until told
// otherwise".
func ReserveFor(seconds int64) time.Duration {
	const longest = int64(math.MaxInt64 / time.Second)
	return time.Duration(min(max(seconds, 0), longest)) * time.Second
}
