package gameserversets

import (
	"fmt"
	"math"
	"math/big"
	"slices"
	"testing"
	"time"

	"agones.dev/agones/pkg/apis"
	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"pgregory.net/rapid"

	"github.com/ponyo877/agones-lite/pkg/gameservers"
)

// These tests state what must hold for any GameServerSet and let rapid look
// for one that breaks it. A failure is printed as the smallest such input.

// knownStates is every state the GameServer controller writes.
var knownStates = []agonesv1.GameServerState{
	portAllocation, creating, agonesv1.GameServerStateStarting, agonesv1.GameServerStateScheduled,
	requestReady, ready, reserved, allocated, shutdown, unhealthy, failed,
}

// anyState is one of those, or a state nobody knows.
func anyState() *rapid.Generator[agonesv1.GameServerState] {
	return rapid.SampledFrom(append(slices.Clone(knownStates), "Hibernating"))
}

// aCrowd draws up to twelve GameServers that tie in every way a removal
// looks at: they share a few nodes, moments of creation and amounts of room.
// Some are on no node, or on one that is gone. Only the names all differ.
func aCrowd(t *rapid.T, states *rapid.Generator[agonesv1.GameServerState]) []*agonesv1.GameServer {
	servers := make([]*agonesv1.GameServer, rapid.IntRange(0, 12).Draw(t, "servers"))
	for i := range servers {
		servers[i] = aGameServer(t, fmt.Sprintf("gs-%d", i), states)
	}
	return servers
}

func aGameServer(t *rapid.T, name string, states *rapid.Generator[agonesv1.GameServerState]) *agonesv1.GameServer {
	gs := server(name, states.Draw(t, "state"),
		on(rapid.SampledFrom([]string{"", "gone", "n1", "n2", "n3"}).Draw(t, "node")),
		created(noon.Add(time.Duration(rapid.IntRange(0, 2).Draw(t, "born"))*time.Second)),
		erroredAt(rapid.SampledFrom(failureStamps).Draw(t, "erroredAt")))
	maybe(t, "rooms", gs, counter("rooms", rapid.Int64Range(0, 3).Draw(t, "roomsTaken"), 3))
	maybe(t, "players", gs, list("players", 3, []string{"ann", "bob", "cy"}[:rapid.IntRange(0, 3).Draw(t, "playersIn")]...))
	maybe(t, "beingDeleted", gs, beingDeleted)
	return gs
}

// failureStamps are the times a GameServer may say it failed at: just
// inside the delay before it is replaced, just outside it, not at all, and
// in a way nobody reads.
var failureStamps = []string{
	noon.Add(-29 * time.Second).Format(time.RFC3339),
	noon.Add(-30 * time.Second).Format(time.RFC3339),
	"",
	"a while ago",
}

// maybe applies opt to gs when rapid says so.
func maybe(t *rapid.T, label string, gs *agonesv1.GameServer, opt option) {
	if rapid.Bool().Draw(t, label) {
		opt(gs)
	}
}

// aSet draws a GameServerSet that wants up to most GameServers.
func aSet(t *rapid.T, most int) *agonesv1.GameServerSet {
	set := setOf(int32(rapid.IntRange(0, most).Draw(t, "replicas")))
	set.Spec.Scheduling = rapid.SampledFrom([]apis.SchedulingStrategy{apis.Packed, apis.Distributed}).Draw(t, "scheduling")
	set.Spec.Priorities = somePriorities(t)
	return set
}

// somePriorities draws what a Fleet may say about which GameServers go
// first: nothing, or the room in a Counter and a List. Admission lets the
// order be left out.
func somePriorities(t *rapid.T) []agonesv1.Priority {
	keys := []agonesv1.Priority{
		{Type: agonesv1.GameServerPriorityCounter, Key: "rooms"},
		{Type: agonesv1.GameServerPriorityList, Key: "players"},
	}
	orders := []string{agonesv1.GameServerPriorityAscending, agonesv1.GameServerPriorityDescending, ""}
	priorities := rapid.SliceOfN(rapid.SampledFrom(keys), 0, 2).Draw(t, "priorities")
	for i := range priorities {
		priorities[i].Order = rapid.SampledFrom(orders).Draw(t, "order")
	}
	return priorities
}

// someCounts draws how full the three nodes are. A node may be missing.
func someCounts(t *rapid.T) map[string]gameservers.NodeCount {
	fill := rapid.Int64Range(0, 2)
	return rapid.MapOfN(rapid.SampledFrom([]string{"n1", "n2", "n3"}), rapid.Custom(func(t *rapid.T) gameservers.NodeCount {
		return gameservers.NodeCount{Ready: fill.Draw(t, "ready"), Allocated: fill.Draw(t, "allocated")}
	}), 0, 3).Draw(t, "counts")
}

// census counts the GameServers of a set one by one, the way upstream's
// computeReconciliationAction tells them apart.
type census struct {
	inSession int // Allocated or Reserved: counted, and never to be removed
	spare     int // counted, and removable
	broken    int // not counted: to be replaced
}

func (c census) counted() int { return c.inSession + c.spare }

func countUp(servers []*agonesv1.GameServer, now time.Time) census {
	var c census
	for _, gs := range servers {
		switch {
		case gs.DeletionTimestamp != nil || gs.Status.State == shutdown:
			// on its way out
		case gs.Status.State == allocated || gs.Status.State == reserved:
			c.inSession++
		case isBroken(gs, now):
			c.broken++
		default:
			c.spare++
		}
	}
	return c
}

// isBroken says what broken says, in as many words as it takes.
func isBroken(gs *agonesv1.GameServer, now time.Time) bool {
	if gs.Status.State == unhealthy {
		return true
	}
	if gs.Status.State != failed {
		return false
	}
	at, err := time.Parse(time.RFC3339, gs.Annotations[agonesv1.GameServerErroredAtAnnotation])
	return err != nil || !now.Before(at.Add(30*time.Second))
}

// A session must outlive a scale-down, and a GameServer that is already
// going must not be told to go a second time.
func TestPlanOnlyRemovesGameServersThatAreIdleAndStillThere(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		servers, set := aCrowd(t, anyState()), aSet(t, 14)

		actions := Plan(set, servers, someCounts(t), noon)

		seen := map[*agonesv1.GameServer]bool{}
		for _, gs := range actions.Delete {
			require.Contains(t, servers, gs, "Plan removes only what it was given")
			require.False(t, seen[gs], "%s is removed twice", gs.Name)
			seen[gs] = true
			assert.NotContains(t, []agonesv1.GameServerState{allocated, reserved, shutdown}, gs.Status.State, gs.Name)
			assert.Nil(t, gs.DeletionTimestamp, "%s is being deleted already", gs.Name)
		}
	})
}

// Scaling down takes the surplus and no more, and never while GameServers
// are missing. Only broken GameServers go whatever the size.
func TestPlanRemovesNoMoreThanTheSurplusAndCreatesNoMoreThanAreMissing(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		servers, set := aCrowd(t, anyState()), aSet(t, 14)
		have := countUp(servers, noon)
		surplus := have.counted() - int(set.Spec.Replicas)

		actions := Plan(set, servers, someCounts(t), noon)

		working := 0
		for _, gs := range actions.Delete {
			if !isBroken(gs, noon) {
				working++
			}
		}
		assert.LessOrEqual(t, working, max(surplus, 0), "working GameServers removed")
		assert.Equal(t, have.broken, len(actions.Delete)-working, "broken GameServers removed")
		assert.Equal(t, max(-surplus, 0), actions.Create, "GameServers created")
	})
}

// One pass does a limited amount of work, however far the set is from its size.
func TestPlanStaysWithinTheLimitsOfOnePass(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		state := rapid.SampledFrom([]agonesv1.GameServerState{ready, unhealthy, creating}).Draw(t, "state")
		servers := append(aCrowd(t, anyState()), many(rapid.IntRange(0, 150).Draw(t, "more"), state)...)

		actions := Plan(aSet(t, 300), servers, someCounts(t), noon)

		assert.LessOrEqual(t, actions.Create, maxCreations)
		assert.LessOrEqual(t, len(actions.Delete), maxDeletions)
	})
}

// A set on its way out must not grow: whatever it created would have to be
// removed again at once.
func TestPlanCreatesNothingForASetThatIsBeingDeleted(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		servers, set := aCrowd(t, anyState()), aSet(t, 14)
		set.DeletionTimestamp = &metav1.Time{Time: noon}

		actions := Plan(set, servers, someCounts(t), noon)

		assert.Zero(t, actions.Create)
	})
}

// The store hands GameServers over in no particular order. Which of them a
// scale-down takes must not follow from that, or two passes over the same
// set could take different ones.
func TestPlanGivesTheSameAnswerForAnyOrderOfTheGameServers(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		servers, set, counts := aCrowd(t, anyState()), aSet(t, 14), someCounts(t)
		want := Plan(set, servers, counts, noon)

		got := Plan(set, rapid.Permutation(servers).Draw(t, "shuffled"), counts, noon)

		assert.Equal(t, want.Create, got.Create)
		assert.Equal(t, names(want.Delete), names(got.Delete))
	})
}

// The same for the order alone, which the autoscaler also asks for.
func TestSortForRemovalGivesTheSameOrderForAnyOrderOfTheGameServers(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		servers, set, counts := aCrowd(t, anyState()), aSet(t, 0), someCounts(t)
		want := SortForRemoval(set.Spec.Scheduling, servers, counts, set.Spec.Priorities)

		got := SortForRemoval(set.Spec.Scheduling, rapid.Permutation(servers).Draw(t, "shuffled"), counts, set.Spec.Priorities)

		assert.Equal(t, names(want), names(got))
		assert.ElementsMatch(t, servers, got, "sorting neither adds nor drops a GameServer")
	})
}

// world is a set's GameServers and a clock: a stand-in for the store, the
// GameServer controller and the agents.
type world struct {
	servers []*agonesv1.GameServer
	now     time.Time
	born    int
}

// carryOut does what the controller does with a plan: new GameServers start
// up, broken ones are deleted, and the others are told to shut down.
func (w *world) carryOut(actions Actions) {
	for range actions.Create {
		w.servers = append(w.servers, server(fmt.Sprintf("new-%d", w.born), creating))
		w.born++
	}
	for _, gs := range actions.Delete {
		if next, ok := gameservers.Next(gs.Status.State, gameservers.ScaledDown); ok {
			gs.Status.State = next
			continue
		}
		beingDeleted(gs)
	}
}

// moveOn lets a minute pass: what was shutting down is gone, and rapid
// decides which of the GameServers that were starting up are Ready now.
func (w *world) moveOn(t *rapid.T) {
	w.now = w.now.Add(time.Minute)
	w.servers = slices.DeleteFunc(w.servers, func(gs *agonesv1.GameServer) bool { return gs.IsBeingDeleted() })
	for _, gs := range w.servers {
		if gs.IsBeforeReady() && rapid.Bool().Draw(t, "ready") {
			gs.Status.State = ready
		}
	}
}

// A set is done when it has the GameServers it asks for and Plan leaves it
// alone. It must get there in a few passes and then stay: a plan that kept
// changing its mind would create and remove GameServers for ever.
func TestCarryingOutThePlanPassAfterPassSettlesAtTheSize(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		w := &world{servers: aCrowd(t, rapid.SampledFrom(knownStates)), now: noon}
		set, counts := aSet(t, 200), someCounts(t)
		// Enough passes to create every GameServer 64 at a time, and three
		// more: broken ones hold back a surplus for one pass, and a failed
		// one waits out its delay.
		passes := 3 + (int(set.Spec.Replicas)+len(w.servers))/maxCreations

		for range passes {
			w.carryOut(Plan(set, w.servers, counts, w.now))
			w.moveOn(t)
		}

		have := countUp(w.servers, w.now)
		assert.Equal(t, max(int(set.Spec.Replicas), have.inSession), have.counted(), "GameServers after %d passes", passes)
		assert.Zero(t, have.broken, "broken GameServers left")
		assert.Equal(t, Actions{}, Plan(set, w.servers, counts, w.now), "the plan after it settled")
	})
}

// The status is what the Fleet and the autoscaler steer by, so it must be
// what counting the GameServers one by one gives.
func TestStatusIsWhatCountingOneByOneGives(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		servers := aCrowd(t, anyState())
		for _, gs := range servers {
			maybe(t, "sessions", gs, counter("sessions", aLargeNumber().Draw(t, "count"), aLargeNumber().Draw(t, "capacity")))
		}
		set := setOf(3)
		set.Spec.Template.Spec.Counters = map[string]agonesv1.CounterStatus{"unused": {}}
		set.Spec.Template.Spec.Lists = map[string]agonesv1.ListStatus{"unused": {}}

		status := Status(set, servers)

		assert.Equal(t, recount(servers), status)
	})
}

// aLargeNumber is a count that a sum of a few may or may not get past the
// end of an int64 with.
func aLargeNumber() *rapid.Generator[int64] {
	return rapid.SampledFrom([]int64{0, 1, 7, math.MaxInt64 / 3, math.MaxInt64 - 1, math.MaxInt64})
}

// recount is Status the slow way: every number is counted on its own, and
// sums are made without an upper end and cut to an int64 afterwards.
func recount(servers []*agonesv1.GameServer) agonesv1.GameServerSetStatus {
	there := func(gs *agonesv1.GameServer) bool { return gs.DeletionTimestamp == nil && gs.Status.State != shutdown }
	in := func(state agonesv1.GameServerState) func(*agonesv1.GameServer) bool {
		return func(gs *agonesv1.GameServer) bool { return there(gs) && gs.Status.State == state }
	}
	status := agonesv1.GameServerSetStatus{
		Replicas:          int32(count(servers, there)),
		ReadyReplicas:     int32(count(servers, in(ready))),
		ReservedReplicas:  int32(count(servers, in(reserved))),
		AllocatedReplicas: int32(count(servers, in(allocated))),
		ShutdownReplicas:  int32(len(servers) - count(servers, there)),
		Counters:          map[string]agonesv1.AggregatedCounterStatus{"unused": {}},
		Lists:             map[string]agonesv1.AggregatedListStatus{"unused": {}},
	}
	for _, name := range []string{"rooms", "sessions"} {
		if total, found := recountCounter(servers, name, there, in(allocated)); found {
			status.Counters[name] = total
		}
	}
	if total, found := recountList(servers, "players", there, in(allocated)); found {
		status.Lists["players"] = total
	}
	return status
}

func count(servers []*agonesv1.GameServer, is func(*agonesv1.GameServer) bool) int {
	n := 0
	for _, gs := range servers {
		if is(gs) {
			n++
		}
	}
	return n
}

func recountCounter(servers []*agonesv1.GameServer, name string, there, inUse func(*agonesv1.GameServer) bool) (total agonesv1.AggregatedCounterStatus, found bool) {
	of := func(pick func(agonesv1.CounterStatus) int64, is func(*agonesv1.GameServer) bool) int64 {
		return sum(servers, func(gs *agonesv1.GameServer) (int64, bool) {
			c, has := gs.Status.Counters[name]
			found = found || (has && there(gs))
			return pick(c), has && is(gs)
		})
	}
	counts := func(c agonesv1.CounterStatus) int64 { return c.Count }
	capacity := func(c agonesv1.CounterStatus) int64 { return c.Capacity }
	return agonesv1.AggregatedCounterStatus{
		Count: of(counts, there), Capacity: of(capacity, there),
		AllocatedCount: of(counts, inUse), AllocatedCapacity: of(capacity, inUse),
	}, found
}

func recountList(servers []*agonesv1.GameServer, name string, there, inUse func(*agonesv1.GameServer) bool) (total agonesv1.AggregatedListStatus, found bool) {
	of := func(pick func(agonesv1.ListStatus) int64, is func(*agonesv1.GameServer) bool) int64 {
		return sum(servers, func(gs *agonesv1.GameServer) (int64, bool) {
			l, has := gs.Status.Lists[name]
			found = found || (has && there(gs))
			return pick(l), has && is(gs)
		})
	}
	values := func(l agonesv1.ListStatus) int64 { return int64(len(l.Values)) }
	capacity := func(l agonesv1.ListStatus) int64 { return l.Capacity }
	return agonesv1.AggregatedListStatus{
		Count: of(values, there), Capacity: of(capacity, there),
		AllocatedCount: of(values, inUse), AllocatedCapacity: of(capacity, inUse),
	}, found
}

// sum adds up what of returns for the GameServers it accepts. A sum past
// the end of an int64 is the largest int64.
func sum(servers []*agonesv1.GameServer, of func(*agonesv1.GameServer) (n int64, ok bool)) int64 {
	total := new(big.Int)
	for _, gs := range servers {
		if n, ok := of(gs); ok {
			total.Add(total, big.NewInt(n))
		}
	}
	if !total.IsInt64() {
		return math.MaxInt64
	}
	return total.Int64()
}
