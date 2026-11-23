package fleets

import (
	"fmt"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	appsv1 "k8s.io/api/apps/v1"
)

// sizes holds the spec.replicas planned for GameServerSets, by name. A set
// that is not in it keeps the size it has.
type sizes map[string]int32

// of returns the size planned for set.
func (s sizes) of(set *agonesv1.GameServerSet) int32 {
	if size, planned := s[set.Name]; planned {
		return size
	}
	return set.Spec.Replicas
}

// resize returns the size the active set should have now, and new sizes for
// the rest. While the rest still has GameServers to give up, the Fleet's
// deployment strategy says how fast the two trade places.
//
// Upstream: applyDeploymentStrategy in pkg/fleets/controller.go.
func resize(fleet *agonesv1.Fleet, active *agonesv1.GameServerSet, rest []*agonesv1.GameServerSet) (int32, sizes, error) {
	switch {
	case len(rest) == 0:
		return fleet.Spec.Replicas, sizes{}, nil
	case agonesv1.SumGameServerSets(rest, specReplicas) == 0:
		// The rest has given up all it can. What it still holds is in use,
		// and the active set makes up the difference.
		return max(fleet.Spec.Replicas-agonesv1.SumGameServerSets(rest, inUse), 0), sizes{}, nil
	case fleet.Spec.Strategy.Type == appsv1.RecreateDeploymentStrategyType:
		replicas, shrunk := recreate(fleet, rest)
		return replicas, shrunk, nil
	case fleet.Spec.Strategy.Type == appsv1.RollingUpdateDeploymentStrategyType:
		return rollingUpdate(fleet, active, rest)
	}
	return 0, nil, fmt.Errorf("unexpected deployment strategy type: %s", fleet.Spec.Strategy.Type)
}

// recreate empties the rest in one go. The active set gets the Fleet's whole
// size at once, less the Allocated GameServers that stay behind in the rest.
//
// Upstream: recreateDeployment in pkg/fleets/controller.go.
func recreate(fleet *agonesv1.Fleet, rest []*agonesv1.GameServerSet) (int32, sizes) {
	shrunk := sizes{}
	for _, set := range rest {
		if set.Spec.Replicas != 0 {
			shrunk[set.Name] = 0
		}
	}
	allocated := agonesv1.SumGameServerSets(rest, allocatedReplicas)
	return fleet.LowerBoundReplicas(fleet.Spec.Replicas - allocated), shrunk
}

// What SumGameServerSets adds up.

func specReplicas(set *agonesv1.GameServerSet) int32 { return set.Spec.Replicas }

func readyReplicas(set *agonesv1.GameServerSet) int32 { return set.Status.ReadyReplicas }

func allocatedReplicas(set *agonesv1.GameServerSet) int32 { return set.Status.AllocatedReplicas }

// inUse is the GameServers of a set that have, or are held for, a session.
// No scaling down removes them.
func inUse(set *agonesv1.GameServerSet) int32 {
	return set.Status.ReservedReplicas + set.Status.AllocatedReplicas
}
