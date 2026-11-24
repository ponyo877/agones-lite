package fleets

import (
	"fmt"
	"math"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// rollingUpdate replaces the rest with the active set a step at a time, so
// that the Fleet keeps serving meanwhile. Each pass grows the active set by
// maxSurge and shrinks the rest as far as maxUnavailable allows.
//
// Upstream: rollingUpdateDeployment in pkg/fleets/controller.go.
func rollingUpdate(fleet *agonesv1.Fleet, active *agonesv1.GameServerSet, rest []*agonesv1.GameServerSet) (int32, sizes, error) {
	replicas, err := grow(fleet, active, rest)
	if err != nil {
		return 0, nil, err
	}
	shrunk, err := shrink(fleet, active, rest)
	if err != nil {
		return 0, nil, err
	}
	return replicas, shrunk, nil
}

// grow returns the size of the active set after one more step up.
//
// Upstream: rollingUpdateActive in pkg/fleets/controller.go. It differs in
// how it holds the Fleet to the surge, in two places that a rollout played
// through by rapid ran into; see running and the comment at its use.
func grow(fleet *agonesv1.Fleet, active *agonesv1.GameServerSet, rest []*agonesv1.GameServerSet) (int32, error) {
	// Allocated GameServers in the rest are never removed, so the active set
	// can only have what they leave of the Fleet's size.
	allocated := agonesv1.SumGameServerSets(rest, allocatedReplicas)
	room := fleet.LowerBoundReplicas(fleet.Spec.Replicas - allocated)
	switch {
	case active.Spec.Replicas != active.Status.Replicas:
		return active.Spec.Replicas, nil // the last step is still under way
	case active.Spec.Replicas >= room:
		return room, nil // nothing left to grow into
	}
	surge, err := maxSurge(fleet)
	if err != nil {
		return 0, err
	}
	replicas := fleet.UpperBoundReplicas(active.Spec.Replicas + surge)
	// Together with the rest, the Fleet may be over its size by the surge
	// and no more. Being over holds the active set back where it is.
	//
	// Upstream takes away from the active set what it is over, down to
	// nothing: a Fleet made smaller in the middle of a rollout loses its
	// new GameServers and keeps the old ones.
	if over := agonesv1.SumGameServerSets(rest, running) + replicas - (fleet.Spec.Replicas + surge); over > 0 {
		replicas = max(replicas-over, active.Spec.Replicas)
	}
	return min(replicas, room), nil
}

// running is how many GameServers a set has, or will have once it has made
// the ones it is short of.
//
// Upstream counts what the set has now. A set of the rest that is short of
// its size then lets the active set grow into the room it is about to
// fill itself, and the Fleet ends up over the surge.
func running(set *agonesv1.GameServerSet) int32 {
	return max(set.Spec.Replicas, set.Status.Replicas)
}

// shrink returns new sizes for the sets of the rest that can give up
// GameServers in this pass.
//
// Upstream: rollingUpdateRestFixedOnReadyRollingUpdateFix in
// pkg/fleets/controller_rollingupdatefix.go, which is modelled on
// reconcileOldReplicaSets in the Deployment controller of Kubernetes.
func shrink(fleet *agonesv1.Fleet, active *agonesv1.GameServerSet, rest []*agonesv1.GameServerSet) (sizes, error) {
	unavailable, err := maxUnavailable(fleet)
	if err != nil {
		return nil, err
	}
	// The Fleet has to keep minAvailable GameServers Ready. The ones it has
	// above that are spare.
	minAvailable := min(fleet.Status.ReadyReplicas-unavailable, fleet.Spec.Replicas)
	ready := active.Status.ReadyReplicas + agonesv1.SumGameServerSets(rest, readyReplicas)
	spare := ready - minAvailable
	// New GameServers have to be up before old ones go: while the active set
	// is waiting for as many as are spare, or more, the rest stays as it is.
	maxScaledDown := spare - missing(active)
	shrunk := sizes{}
	if maxScaledDown <= 0 {
		return shrunk, nil
	}
	dropUnhealthy(rest, shrunk, maxScaledDown)
	scaleDown(rest, shrunk, spare) // losing every spare one still leaves minAvailable
	return shrunk, nil
}

// missing is how many of the GameServers the active set asks for are not up.
// Ones that are shutting down are not missed either. Upstream calls it
// newGSSUnavailablePodCount.
func missing(active *agonesv1.GameServerSet) int32 {
	s := active.Status
	return active.Spec.Replicas - s.ReadyReplicas - s.ReservedReplicas - s.AllocatedReplicas - s.ShutdownReplicas
}

// dropUnhealthy lowers the rest by the GameServers that are not Ready,
// Reserved or Allocated, limit at most in all. Nobody can play on those, so
// they go before any that work.
//
// Upstream: cleanupUnhealthyReplicasRollingUpdateFix in
// pkg/fleets/controller_rollingupdatefix.go.
func dropUnhealthy(rest []*agonesv1.GameServerSet, shrunk sizes, limit int32) {
	for _, set := range rest {
		healthy := set.Status.ReadyReplicas + set.Status.AllocatedReplicas + set.Status.ReservedReplicas
		cut := min(set.Spec.Replicas-healthy, limit)
		if cut <= 0 {
			continue
		}
		shrunk[set.Name] = set.Spec.Replicas - cut
		limit -= cut
	}
}

// scaleDown lowers the rest by limit GameServers at most in all, one set
// after the other.
//
// Upstream: the last loop of rollingUpdateRestFixedOnReadyRollingUpdateFix.
// It stops at a set that holds more sessions than its size, which is not
// at its size until they end. The Ready GameServers of the sets after it
// then stay, and take new sessions, for as long as the old ones last.
func scaleDown(rest []*agonesv1.GameServerSet, shrunk sizes, limit int32) {
	for _, set := range rest {
		size := shrunk.of(set)
		switch {
		case limit <= 0:
			return
		case size <= 0:
			continue
		case inUse(set) >= size:
			// It holds as many sessions as its size, or more. They stay
			// whatever the size says and nothing else of it does, so the
			// set is done, and nothing is lost by saying so.
			shrunk[set.Name] = 0
			continue
		case size != set.Status.Replicas:
			return // not at its size yet: wait for it before going further
		case set.Status.ShutdownReplicas != 0:
			continue // the GameServers it last gave up are not gone yet
		}
		cut := min(size, limit)
		limit -= cut
		shrunk[set.Name] = size - cut
	}
}

// maxSurge is how many GameServers the active set gains in one step. A
// percentage is of the Fleet's size, rounded up.
func maxSurge(fleet *agonesv1.Fleet) (int32, error) {
	surge, err := intstr.GetScaledValueFromIntOrPercent(rolling(fleet).MaxSurge, int(fleet.Spec.Replicas), true)
	if err != nil {
		return 0, fmt.Errorf("parsing maxSurge: %w", err)
	}
	return count(surge), nil
}

// count brings a number of GameServers that was worked out as an int back
// to an int32, held at the largest there is. A share may be more than the
// whole: 150% of a very large Fleet is more than an int32 holds, and cut
// to size it would come out negative.
func count(n int) int32 {
	return int32(min(n, math.MaxInt32)) //nolint:gosec // held at the limit
}

// maxUnavailable is how many Ready GameServers the Fleet may lose in one
// step. A percentage is of the Ready GameServers at the last pass, rounded
// down. It is at least 1, or a small Fleet would never get anywhere, and at
// most the Fleet's size.
func maxUnavailable(fleet *agonesv1.Fleet) (int32, error) {
	unavailable, err := intstr.GetScaledValueFromIntOrPercent(rolling(fleet).MaxUnavailable, int(fleet.Status.ReadyReplicas), false)
	if err != nil {
		return 0, fmt.Errorf("parsing maxUnavailable: %w", err)
	}
	if unavailable == 0 {
		unavailable = 1
	}
	return min(count(unavailable), fleet.Spec.Replicas), nil
}

// rolling returns the Fleet's rolling update settings, empty when it has
// none. Admission fills them in, but upstream panics on a Fleet without.
func rolling(fleet *agonesv1.Fleet) appsv1.RollingUpdateDeployment {
	if fleet.Spec.Strategy.RollingUpdate == nil {
		return appsv1.RollingUpdateDeployment{}
	}
	return *fleet.Spec.Strategy.RollingUpdate
}
