package gameserverallocations

import (
	"fmt"
	"math/rand/v2"
	"testing"

	"agones.dev/agones/pkg/apis"
	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	allocationv1 "agones.dev/agones/pkg/apis/allocation/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ponyo877/scrim/pkg/gameservers"
)

// BenchmarkFind measures the choice of one GameServer among many that
// would all do: a Fleet's worth of Ready ones, a tenth of them in use,
// spread over nodes of a hundred each. Every allocation makes this choice.
//
//	go test ./pkg/gameserverallocations/ -run '^$' -bench Find
func BenchmarkFind(b *testing.B) {
	for _, strategy := range []apis.SchedulingStrategy{apis.Packed, apis.Distributed} {
		for _, size := range []int{1000, 10000} {
			b.Run(fmt.Sprintf("%s/among-%d", strategy, size), func(b *testing.B) { benchmarkFind(b, strategy, size) })
		}
	}
}

func benchmarkFind(b *testing.B, strategy apis.SchedulingStrategy, size int) {
	gsa := request(b, allocationv1.GameServerAllocationSpec{
		Scheduling: strategy,
		Selectors:  []allocationv1.GameServerSelector{{LabelSelector: metav1.LabelSelector{MatchLabels: role}}},
	})
	servers := aFleet(size)
	counts := gameservers.CountByNode(servers)
	for b.Loop() {
		if _, found := Find(gsa, servers, counts, rand.IntN); !found {
			b.Fatal("nothing found")
		}
	}
}

// aFleet is size GameServers as a Fleet has them: the same labels, names
// that say nothing, every tenth one Allocated.
func aFleet(size int) []*agonesv1.GameServer {
	servers := make([]*agonesv1.GameServer, size)
	for i := range servers {
		state := ready
		if i%10 == 0 {
			state = allocated
		}
		name := fmt.Sprintf("fleet-abcde-%05d", (i*7919)%size) // not in the order of the nodes
		servers[i] = server(name, state, on(fmt.Sprint("node-", i/100)), labelled(role))
	}
	return servers
}

// BenchmarkFindSpread measures the same choice as BenchmarkFind makes for
// Distributed, made by a draw: compare the two.
//
//	go test ./pkg/gameserverallocations/ -run '^$' -bench 'Find(Spread)?/Distributed'
func BenchmarkFindSpread(b *testing.B) {
	for _, size := range []int{1000, 10000} {
		b.Run(fmt.Sprintf("%s/among-%d", apis.Distributed, size), func(b *testing.B) {
			gsa := request(b, allocationv1.GameServerAllocationSpec{
				Scheduling: apis.Distributed,
				Selectors:  []allocationv1.GameServerSelector{{LabelSelector: metav1.LabelSelector{MatchLabels: role}}},
			})
			servers := aFleet(size)
			counts := gameservers.CountByNode(servers)
			one := inPlaces(servers)
			for b.Loop() {
				if _, found := FindSpread(gsa, counts, one, rand.IntN); !found {
					b.Fatal("nothing found")
				}
			}
		})
	}
}

// inPlaces is the Fleet as the store hands it out one by one, sorted
// beforehand as the store keeps it sorted.
func inPlaces(servers []*agonesv1.GameServer) func(string, agonesv1.GameServerState, int) (*agonesv1.GameServer, bool) {
	type spot struct {
		node  string
		state agonesv1.GameServerState
	}
	spots := map[spot][]*agonesv1.GameServer{}
	for _, gs := range servers {
		at := spot{gs.Status.NodeName, gs.Status.State}
		spots[at] = append(spots[at], gs)
	}
	return func(node string, state agonesv1.GameServerState, place int) (*agonesv1.GameServer, bool) {
		there := spots[spot{node, state}]
		if place >= len(there) {
			return nil, false
		}
		return there[place], true
	}
}
