package fleets

import (
	"fmt"
	"math/rand"
	"slices"
	"testing"
	"time"

	"agones.dev/agones/pkg/apis"
	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/ponyo877/agones-lite/pkg/agones"
)

var noon = time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)

// newFleet is upstream's defaultFixture: 5 replicas, and a rolling update
// with the default maxSurge and maxUnavailable of 25%.
func newFleet(t *testing.T) *agonesv1.Fleet {
	t.Helper()
	require.NoError(t, agones.Setup())
	fleet := &agonesv1.Fleet{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "fleet-1", UID: "1234"},
		Spec:       agonesv1.FleetSpec{Replicas: 5, Scheduling: apis.Packed},
	}
	fleet.ApplyDefaults()
	return fleet
}

// rollingBy sets how far one step of a rolling update goes, as percentages
// like "25%" or as numbers like "3".
func rollingBy(fleet *agonesv1.Fleet, maxSurge, maxUnavailable string) {
	surge, unavailable := intstr.Parse(maxSurge), intstr.Parse(maxUnavailable)
	fleet.Spec.Strategy.RollingUpdate.MaxSurge = &surge
	fleet.Spec.Strategy.RollingUpdate.MaxUnavailable = &unavailable
}

// shape is a GameServerSet reduced to the numbers this package looks at.
type shape struct {
	spec      int32 // spec.replicas
	replicas  int32 // the GameServers it has
	ready     int32
	reserved  int32
	allocated int32
	shutdown  int32 // on their way out, and no longer among replicas
}

// full is a set at its size with every GameServer Ready.
func full(size int32) shape {
	return shape{spec: size, replicas: size, ready: size}
}

// current returns a stored set made from the Fleet's template as it is now.
func current(fleet *agonesv1.Fleet, name string, s shape) *agonesv1.GameServerSet {
	set := fleet.GameServerSet()
	set.Name = name
	set.Spec.Replicas = s.spec
	set.Status = agonesv1.GameServerSetStatus{
		Replicas:          s.replicas,
		ReadyReplicas:     s.ready,
		ReservedReplicas:  s.reserved,
		AllocatedReplicas: s.allocated,
		ShutdownReplicas:  s.shutdown,
	}
	return set
}

// old returns a stored set made from an earlier template.
func old(fleet *agonesv1.Fleet, name string, s shape) *agonesv1.GameServerSet {
	set := current(fleet, name, s)
	set.Spec.Template.Spec.Ports = []agonesv1.GameServerPort{{HostPort: 9999}}
	return set
}

// born sets when a set was created, in minutes after noon.
func born(set *agonesv1.GameServerSet, minute int) *agonesv1.GameServerSet {
	set.CreationTimestamp = metav1.NewTime(noon.Add(time.Duration(minute) * time.Minute))
	return set
}

func list(sets ...*agonesv1.GameServerSet) []*agonesv1.GameServerSet {
	return sets
}

// seenReady records on the Fleet the Ready GameServers of sets, as the
// previous pass would have.
func seenReady(fleet *agonesv1.Fleet, sets ...*agonesv1.GameServerSet) {
	fleet.Status.ReadyReplicas = agonesv1.SumGameServerSets(sets, readyReplicas)
}

// family returns the sets of a Fleet that has been updated a few times in
// a row: some are made from its template as it is now, some from an
// earlier one, and several were created in the same minute. Only the names
// all differ.
func family(random *rand.Rand, fleet *agonesv1.Fleet) []*agonesv1.GameServerSet {
	sets := make([]*agonesv1.GameServerSet, 1+random.Intn(5))
	for i := range sets {
		from := current
		if random.Intn(3) > 0 {
			from = old
		}
		sets[i] = born(from(fleet, fmt.Sprintf("set-%d", i), anyShape(random)), random.Intn(3))
	}
	return sets
}

// anyShape is a small set in any state between full and empty.
func anyShape(random *rand.Rand) shape {
	replicas := int32(random.Intn(6))
	ready := int32(random.Intn(int(replicas) + 1))
	return shape{
		spec:      int32(random.Intn(6)),
		replicas:  replicas,
		ready:     ready,
		allocated: int32(random.Intn(int(replicas-ready) + 1)),
		shutdown:  int32(random.Intn(2)),
	}
}

// shuffled returns the sets in another order.
func shuffled(random *rand.Rand, sets []*agonesv1.GameServerSet) []*agonesv1.GameServerSet {
	out := slices.Clone(sets)
	random.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}
