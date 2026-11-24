package gameserverallocations

import (
	"fmt"
	"math/rand/v2"
	"testing"

	"agones.dev/agones/pkg/apis"
	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	allocationv1 "agones.dev/agones/pkg/apis/allocation/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ponyo877/agones-lite/pkg/gameservers"
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
