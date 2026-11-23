package gameserversets

import (
	"slices"
	"testing"
	"time"

	"agones.dev/agones/pkg/apis"
	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ponyo877/agones-lite/pkg/gameservers"
)

// The cases upstream has in TestComputeReconciliationAction come first, with
// its test limits of 3 replaced by the real ones.
func TestPlanCountsWhatToCreateAndRemove(t *testing.T) {
	tests := map[string]struct {
		servers  []*agonesv1.GameServer
		replicas int32
		create   int
		remove   int
	}{
		"an empty set that wants nothing is left alone": {},
		"missing GameServers are created": {
			servers: of(ready), replicas: 3,
			create: 2,
		},
		"at most 64 are created in one pass": {
			servers: of(ready), replicas: 100,
			create: 64,
		},
		"creation slows down near 5000 GameServers that are still starting up": {
			servers: many(4999, creating), replicas: 6000,
			create: 1,
		},
		"nothing is created while 5000 are still starting up": {
			servers: many(5000, portAllocation), replicas: 6000,
		},
		"surplus GameServers are removed": {
			servers: of(ready, reserved, ready), replicas: 1,
			remove: 2,
		},
		"at most 64 are removed in one pass": {
			servers: many(70, ready), replicas: 1,
			remove: 64,
		},
		"Allocated GameServers are never removed": {
			servers: of(ready, allocated, allocated), replicas: 0,
			remove: 1,
		},
		"Reserved GameServers are never removed": {
			servers: of(ready, reserved, reserved), replicas: 0,
			remove: 1,
		},
		"GameServers that are being deleted are replaced without waiting": {
			servers:  []*agonesv1.GameServer{server("", unhealthy, beingDeleted), server("", unhealthy, beingDeleted), server("", ready)},
			replicas: 4,
			create:   3,
		},
		"Unhealthy GameServers are replaced": {
			servers: of(ready, unhealthy, unhealthy), replicas: 3,
			create: 2, remove: 2,
		},
		"Error GameServers are replaced": {
			servers: of(ready, failed, failed), replicas: 3,
			create: 2, remove: 2,
		},
		"broken GameServers count toward the 64 removals": {
			servers: slices.Concat(of(ready), many(70, unhealthy)), replicas: 3,
			create: 2, remove: 64,
		},

		// Not covered by upstream's tests.
		"more than 5000 starting up does not make the count negative": {
			servers: many(5001, creating), replicas: 6000,
		},
		"a GameServer that asked to be Ready is no longer starting up": {
			servers: many(5000, requestReady), replicas: 6000,
			create: 64,
		},
		"a GameServer waiting out its Error delay counts as starting up": {
			servers: many(5000, failed, erroredAt("2026-11-23T11:59:59Z")), replicas: 6000,
		},
		"Shutdown GameServers are replaced without waiting": {
			servers: of(ready, shutdown), replicas: 2,
			create: 1,
		},
		"an Allocated GameServer that is being deleted no longer counts": {
			servers: []*agonesv1.GameServer{server("", allocated, beingDeleted)}, replicas: 1,
			create: 1,
		},
		"a state nobody knows counts as alive": {
			servers: of("Hibernating"), replicas: 1,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			actions := Plan(setOf(tc.replicas), tc.servers, nil, noon)

			assert.Equal(t, tc.create, actions.Create, "GameServers to create")
			assert.Len(t, actions.Delete, tc.remove, "GameServers to remove")
		})
	}
}

func TestPlanWaitsThirtySecondsBeforeReplacingAnErrorGameServer(t *testing.T) {
	tests := map[string]struct {
		erroredAt string
		replaced  bool
	}{
		"it failed 29 seconds ago":         {erroredAt: "2026-11-23T11:59:31Z"},
		"it failed 30 seconds ago":         {erroredAt: "2026-11-23T11:59:30Z", replaced: true},
		"it failed in another time zone":   {erroredAt: "2026-11-23T20:59:31+09:00"},
		"it does not say when it failed":   {erroredAt: "", replaced: true},
		"it says so in a way nobody reads": {erroredAt: "half a minute ago", replaced: true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			gs := server("gs", failed, erroredAt(tc.erroredAt))

			actions := Plan(setOf(1), []*agonesv1.GameServer{gs}, nil, noon)

			if tc.replaced {
				assert.Equal(t, Actions{Create: 1, Delete: []*agonesv1.GameServer{gs}}, actions)
				return
			}
			assert.Equal(t, Actions{}, actions, "it still counts, so nothing takes its place yet")
		})
	}
}

func TestPlanAddsNothingToASetThatIsBeingDeleted(t *testing.T) {
	set := setOf(3)
	set.DeletionTimestamp = &metav1.Time{Time: noon}
	servers := []*agonesv1.GameServer{server("well", ready), server("sick", unhealthy)}

	actions := Plan(set, servers, nil, noon)

	assert.Zero(t, actions.Create)
	assert.Equal(t, []string{"sick"}, names(actions.Delete), "removals go on as usual")
}

// Upstream: "test packed scale down" in TestComputeReconciliationAction.
func TestPlanEmptiesTheLeastFullNodeWhenPacked(t *testing.T) {
	set := setOf(2)
	set.Spec.Scheduling = apis.Packed
	servers := []*agonesv1.GameServer{
		server("gs1", ready, on("node3")),
		server("gs2", ready, on("node1")),
		server("gs3", ready, on("node3")),
		server("gs4", ready),
	}
	counts := map[string]gameservers.NodeCount{"node1": {Ready: 1}, "node3": {Ready: 2}}

	actions := Plan(set, servers, counts, noon)

	assert.Zero(t, actions.Create)
	assert.Equal(t, []string{"gs4", "gs2"}, names(actions.Delete))
}

// Upstream: "test distributed scale down" in TestComputeReconciliationAction.
func TestPlanRemovesTheOldestWhenDistributed(t *testing.T) {
	servers := []*agonesv1.GameServer{
		server("gs1", ready, created(noon.Add(10*time.Second))),
		server("gs2", ready, created(noon)),
		server("gs3", ready, created(noon.Add(40*time.Second))),
		server("gs4", ready, created(noon.Add(30*time.Second))),
	}

	actions := Plan(setOf(2), servers, nil, noon)

	assert.Zero(t, actions.Create)
	assert.Equal(t, []string{"gs2", "gs1"}, names(actions.Delete))
}

// Upstream has no test for this. Its code removes one surplus GameServer
// fewer for every broken one it removes, and this pins that down.
func TestEveryBrokenGameServerKeepsOneSurplusGameServerForNow(t *testing.T) {
	healthy := []*agonesv1.GameServer{
		server("first", ready, created(noon.Add(-3*time.Minute))),
		server("second", ready, created(noon.Add(-2*time.Minute))),
		server("third", ready, created(noon.Add(-time.Minute))),
	}
	tests := map[string]struct {
		broken []*agonesv1.GameServer
		remove []string
	}{
		"nothing broken: both surplus GameServers go": {
			remove: []string{"first", "second"},
		},
		"one broken: one of the two stays": {
			broken: []*agonesv1.GameServer{server("sick", unhealthy)},
			remove: []string{"sick", "first"},
		},
		"two broken: both stay": {
			broken: []*agonesv1.GameServer{server("sick", unhealthy), server("stuck", failed)},
			remove: []string{"sick", "stuck"},
		},
		"three broken: still both stay": {
			broken: []*agonesv1.GameServer{server("sick", unhealthy), server("stuck", failed), server("lost", unhealthy)},
			remove: []string{"sick", "stuck", "lost"},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			actions := Plan(setOf(1), slices.Concat(healthy, tc.broken), nil, noon)

			assert.Zero(t, actions.Create)
			assert.Equal(t, tc.remove, names(actions.Delete))
		})
	}
}

func TestPlanLeavesItsArgumentsAlone(t *testing.T) {
	servers := []*agonesv1.GameServer{
		server("newest", ready, created(noon.Add(-time.Second))),
		server("oldest", ready, created(noon.Add(-time.Hour))),
		server("middle", ready, created(noon.Add(-time.Minute))),
	}

	actions := Plan(setOf(1), servers, nil, noon)

	assert.Equal(t, []string{"oldest", "middle"}, names(actions.Delete))
	assert.Equal(t, []string{"newest", "oldest", "middle"}, names(servers), "sorting for removal works on a copy")
}
