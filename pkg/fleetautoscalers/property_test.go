package fleetautoscalers

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"testing"
	"time"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	autoscalingv1 "agones.dev/agones/pkg/apis/autoscaling/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admregv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"pgregory.net/rapid"
)

// These tests state what must hold for any policy and any Fleet and let
// rapid look for a pair that breaks it. A failure is printed as the
// smallest such pair.

// percent is a buffer given as a share, like "30%".
func percent(p int) intstr.IntOrString { return share(fmt.Sprintf("%d%%", p)) }

// aBufferPolicy draws a Buffer policy that upstream's validation accepts:
// a number above zero or a share from 1% to 99%, with limits to match.
func aBufferPolicy(t *rapid.T) autoscalingv1.FleetAutoscalerPolicy {
	least, size := int32(1), percent(rapid.IntRange(1, 99).Draw(t, "share"))
	if rapid.Bool().Draw(t, "asANumber") {
		least = rapid.Int32Range(1, 12).Draw(t, "amount")
		size = amount(least)
	}
	smallest := rapid.Int32Range(least, least+10).Draw(t, "minReplicas")
	if size.Type == intstr.Int && rapid.Bool().Draw(t, "noMinimum") {
		smallest = 0
	}
	largest := rapid.Int32Range(max(smallest, least), least+30).Draw(t, "maxReplicas")
	policy := bufferPolicy(size, smallest, largest)
	require.Empty(t, policy.ValidatePolicy(field.NewPath("policy")), "upstream should accept it")
	return policy
}

// aFleetSize draws how many GameServers of a Fleet are Allocated: a few,
// or as many as a Fleet can have.
func aFleetSize() *rapid.Generator[int32] {
	return rapid.OneOf(rapid.Int32Range(0, 40), rapid.SampledFrom([]int32{math.MaxInt32 - 1, math.MaxInt32}))
}

func desiredBy(t *rapid.T, policy autoscalingv1.FleetAutoscalerPolicy, allocated int32) (replicas int32, limited bool) {
	replicas, limited, _, err := Desired(policy, fleetOf(sized(8, allocated, allocated)), nil, nil, noon, nil)
	require.NoError(t, err)
	return replicas, limited
}

// A Buffer policy names the smallest and the largest size the Fleet may
// have, and whoever wrote it counts on both. Between them, the buffer must
// be free, and not a replica more than that takes: replicas cost money.
func TestBufferStaysWithinItsLimitsAndKeepsJustTheBufferFree(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		policy, allocated := aBufferPolicy(t), aFleetSize().Draw(t, "allocated")

		replicas, limited := desiredBy(t, policy, allocated)

		b := policy.Buffer
		require.GreaterOrEqual(t, replicas, b.MinReplicas)
		require.LessOrEqual(t, replicas, b.MaxReplicas)
		if limited {
			assert.Contains(t, []int32{b.MinReplicas, b.MaxReplicas}, replicas, "a limited answer is one of the limits")
			return
		}
		assert.True(t, bufferIsFree(b.BufferSize, int64(allocated), int64(replicas)), "%d replicas do not leave the buffer free", replicas)
		assert.False(t, bufferIsFree(b.BufferSize, int64(allocated), int64(replicas-1)), "%d replicas would have done", replicas-1)
	})
}

// bufferIsFree reports whether a Fleet of that size, with that many of its
// GameServers Allocated, has the buffer free. It only multiplies.
func bufferIsFree(size intstr.IntOrString, allocated, replicas int64) bool {
	free := replicas - allocated
	if size.Type == intstr.Int {
		return free >= int64(size.IntValue())
	}
	share, _ := intstr.GetScaledValueFromIntOrPercent(&size, 100, true)
	return 100*free >= int64(share)*replicas
}

// More players must never mean fewer GameServers.
func TestBufferNeverAsksForFewerReplicasWhenMoreAreAllocated(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		policy := aBufferPolicy(t)
		fewer, more := aFleetSize().Draw(t, "allocated"), aFleetSize().Draw(t, "allocatedLater")
		if fewer > more {
			fewer, more = more, fewer
		}

		before, _ := desiredBy(t, policy, fewer)
		after, _ := desiredBy(t, policy, more)

		assert.LessOrEqual(t, before, after)
	})
}

// aLargeCount draws the count of a Counter: small, or with more digits
// than a float64 holds.
func aLargeCount() *rapid.Generator[int64] {
	return rapid.OneOf(rapid.Int64Range(1, 1000), rapid.Int64Range(1<<46, math.MaxInt64))
}

// The numbers of a Counter are int64, and a share of them is a division.
// Done in floating point, it is a little off from fifteen digits on, and
// what a 0/0 or a number too large becomes depends on the CPU. So the
// answer must be the one that whole numbers of any size give.
func TestACounterPolicyGrowsByExactlyTheReplicasThatWholeNumbersGive(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		share := rapid.IntRange(1, 99).Draw(t, "share")
		inUse := aLargeCount().Draw(t, "allocated")
		room := rapid.SampledFrom([]int64{1, 3, 7, 1000, 1 << 40}).Draw(t, "capacityOfAReplica")
		// A Fleet of one replica, with every bit of its capacity Allocated.
		tc := capacityCase{
			perReplica: usage{0, room}, replicas: 1, total: usage{inUse, inUse}, allocated: inUse,
			policy: counterPolicy(percent(share), 1, math.MaxInt64),
		}

		replicas, limited, _, err := Desired(tc.policy, counterFleet(tc), nil, nil, noon, nil)

		require.NoError(t, err)
		want, wantLimited := grownInWholeNumbers(inUse, int64(share), room)
		assert.Equal(t, want, replicas)
		assert.Equal(t, wantLimited, limited)
	})
}

// grownInWholeNumbers is the size of that Fleet once the share is free, by
// the rules in capacity.go: the buffer and a capacity are int64 at most, a
// Fleet has int32 replicas at most, and of the replicas it takes only as
// many are added as keep the capacity an int64.
func grownInWholeNumbers(allocated, share, room int64) (replicas int32, limited bool) {
	most := big.NewInt(math.MaxInt64)
	buffer := least(divideUpExactly(times(allocated, share), big.NewInt(100-share)), most)
	extra := divideUpExactly(buffer, big.NewInt(room))
	fit := new(big.Int).Div(new(big.Int).Sub(most, big.NewInt(allocated)), big.NewInt(room))
	if extra.Cmp(fit) > 0 {
		extra, limited = fit, true
	}
	size := least(new(big.Int).Add(extra, big.NewInt(1)), big.NewInt(math.MaxInt32))
	return int32(size.Int64()), limited
}

func times(a, b int64) *big.Int { return new(big.Int).Mul(big.NewInt(a), big.NewInt(b)) }

func least(a, b *big.Int) *big.Int {
	if a.Cmp(b) < 0 {
		return a
	}
	return b
}

// divideUpExactly is a / b rounded up, for numbers that are not negative.
func divideUpExactly(a, b *big.Int) *big.Int {
	quotient, remainder := new(big.Int).DivMod(a, b, new(big.Int))
	if remainder.Sign() > 0 {
		quotient.Add(quotient, big.NewInt(1))
	}
	return quotient
}

// aBufferSize draws a buffer of any kind, the ones admission refuses
// included: it does not look at the policy inside a Schedule.
func aBufferSize() *rapid.Generator[intstr.IntOrString] {
	return rapid.OneOf(
		rapid.Map(rapid.Int32Range(-3, 12), amount),
		rapid.Map(rapid.IntRange(-5, 105), percent),
		rapid.SampledFrom([]intstr.IntOrString{share(""), share("many"), share("50"), share("1e3%"), {Type: 7}}),
	)
}

// The same goes for the shares that should not be there at all: 0%, 100%
// and beyond. What they come to must not depend on the machine, so they
// are errors, and an error is the same everywhere.
func TestAShareThatIsNotFrom1To99PercentIsAnError(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		share := rapid.OneOf(rapid.IntRange(-200, 0), rapid.IntRange(100, 300)).Draw(t, "share")
		policy := rapid.SampledFrom([]autoscalingv1.FleetAutoscalerPolicy{
			bufferPolicy(percent(share), 1, 10),
			counterPolicy(percent(share), 1, 100),
			schedulePolicy(between("", ""), counterPolicy(percent(share), 5, 100)),
		}).Draw(t, "policy")
		tc := capacityCase{perReplica: usage{0, 4}, replicas: 3, total: usage{12, 12}, allocated: rapid.Int64Range(0, 12).Draw(t, "allocated")}

		replicas, limited, applied, err := Desired(policy, counterFleet(tc), nil, nil, noon, nil)

		require.ErrorContains(t, err, "bufferSize should be between 1% and 99%")
		assert.Zero(t, replicas)
		assert.False(t, limited)
		assert.Empty(t, applied)
	})
}

// aNumber draws a number that may be anything: small, below zero, or at
// either end of what its type holds.
func aNumber() *rapid.Generator[int64] {
	return rapid.OneOf(rapid.Int64Range(-2, 12), rapid.SampledFrom([]int64{math.MinInt64, math.MinInt32, math.MaxInt32, math.MaxInt64}))
}

func aSmallNumber() *rapid.Generator[int32] {
	return rapid.OneOf(rapid.Int32Range(-2, 12), rapid.SampledFrom([]int32{math.MinInt32, math.MaxInt32}))
}

// anyPolicy draws a policy the way a store may hold one: of any type or of
// none, with or without the parameters its type needs, and with any policy
// inside a Schedule or a Chain, up to depth deep.
func anyPolicy(depth int) *rapid.Generator[autoscalingv1.FleetAutoscalerPolicy] {
	types := []autoscalingv1.FleetAutoscalerPolicyType{bufferType, webhookType, counterType, listType, scheduleType, chainType, autoscalingv1.WasmPolicyType, "", "Hunch"}
	return rapid.Custom(func(t *rapid.T) autoscalingv1.FleetAutoscalerPolicy {
		policy := autoscalingv1.FleetAutoscalerPolicy{Type: rapid.SampledFrom(types).Draw(t, "type")}
		key := rapid.SampledFrom([]string{rooms, players, "gone", ""}).Draw(t, "key")
		size, smallest, largest := aBufferSize().Draw(t, "bufferSize"), aNumber().Draw(t, "min"), aNumber().Draw(t, "max")
		if rapid.Bool().Draw(t, "hasParameters") {
			policy.Buffer = &autoscalingv1.BufferPolicy{BufferSize: size, MinReplicas: int32(smallest), MaxReplicas: int32(largest)}
			policy.Counter = &autoscalingv1.CounterPolicy{Key: key, BufferSize: size, MinCapacity: smallest, MaxCapacity: largest}
			policy.List = &autoscalingv1.ListPolicy{Key: key, BufferSize: size, MinCapacity: smallest, MaxCapacity: largest}
			policy.Webhook = rapid.SampledFrom(webhookTargets).Draw(t, "webhook")
		}
		if depth > 0 && rapid.Bool().Draw(t, "hasPoliciesInside") {
			policy.Schedule = anySchedule(depth).Draw(t, "schedule")
			policy.Chain = rapid.SliceOfN(rapid.Map(anyPolicy(depth-1), asEntry), 0, 3).Draw(t, "chain")
		}
		return policy
	})
}

func asEntry(policy autoscalingv1.FleetAutoscalerPolicy) autoscalingv1.ChainEntry {
	return entry("entry", policy)
}

var (
	missingURL, badURL, goodURL = "", "not a url", "https://scale.example.com/scale"

	// webhookTargets are the ways a Webhook policy can say, or fail to
	// say, where its review goes.
	webhookTargets = []*autoscalingv1.URLConfiguration{
		{}, {URL: &missingURL}, {URL: &badURL}, {URL: &goodURL},
		{Service: &admregv1.ServiceReference{Name: "scaler"}},
		{URL: &goodURL, Service: &admregv1.ServiceReference{Name: "scaler"}},
	}
)

// anySchedule draws a Schedule whose times may or may not make sense.
func anySchedule(depth int) *rapid.Generator[*autoscalingv1.SchedulePolicy] {
	moments := []metav1.Time{{}, metav1.NewTime(noon.Add(-time.Hour)), metav1.NewTime(noon.Add(time.Hour))}
	return rapid.Custom(func(t *rapid.T) *autoscalingv1.SchedulePolicy {
		s := &autoscalingv1.SchedulePolicy{Policy: anyPolicy(depth-1).Draw(t, "policy")}
		s.Between.Start = rapid.SampledFrom(moments).Draw(t, "start")
		s.Between.End = rapid.SampledFrom(moments).Draw(t, "end")
		s.ActivePeriod = autoscalingv1.ActivePeriod{
			Timezone:  rapid.SampledFrom([]string{"", "UTC", "Asia/Tokyo", "Mars/Olympus_Mons"}).Draw(t, "timezone"),
			StartCron: rapid.SampledFrom([]string{"", "* * * * *", "0 9 * * *", "0 0 31 2 *", "at nine", "TZ=UTC * * * * *"}).Draw(t, "startCron"),
			Duration:  rapid.SampledFrom([]string{"", "1h", "0s", "-5m", "an hour", "2540400h"}).Draw(t, "duration"),
		}
		return s
	})
}

// anyFleet draws a Fleet with the rooms Counter and the players List, or
// without, and with any numbers in them.
func anyFleet(t *rapid.T) *agonesv1.Fleet {
	fleet := fleetOf(sized(aSmallNumber().Draw(t, "replicas"), aSmallNumber().Draw(t, "current"), aSmallNumber().Draw(t, "allocated")))
	total := agonesv1.AggregatedCounterStatus{
		Count: aNumber().Draw(t, "count"), Capacity: aNumber().Draw(t, "capacity"), AllocatedCount: aNumber().Draw(t, "allocatedCount"),
	}
	if rapid.Bool().Draw(t, "inTemplate") {
		one := agonesv1.CounterStatus{Count: aNumber().Draw(t, "countOfOne"), Capacity: aNumber().Draw(t, "capacityOfOne")}
		fleet.Spec.Template.Spec.Counters = map[string]agonesv1.CounterStatus{rooms: one}
		fleet.Spec.Template.Spec.Lists = map[string]agonesv1.ListStatus{players: {Capacity: one.Capacity, Values: values(max(one.Count, 0) % 4)}}
	}
	if rapid.Bool().Draw(t, "inStatus") {
		fleet.Status.Counters = map[string]agonesv1.AggregatedCounterStatus{rooms: total}
		fleet.Status.Lists = map[string]agonesv1.AggregatedListStatus{players: agonesv1.AggregatedListStatus(total)}
	}
	return fleet
}

// someServers draws a few GameServers with numbers of their own.
func someServers(t *rapid.T) []*agonesv1.GameServer {
	servers := make([]*agonesv1.GameServer, rapid.IntRange(0, 4).Draw(t, "servers"))
	for i := range servers {
		state := rapid.SampledFrom([]agonesv1.GameServerState{ready, allocated, reserved}).Draw(t, "state")
		count, capacity := aNumber().Draw(t, "countOfServer"), aNumber().Draw(t, "capacityOfServer")
		servers[i] = server(fmt.Sprintf("gs-%d", i), state, on("n1"), counting(count, capacity))
		servers[i].Status.Lists = map[string]agonesv1.ListStatus{players: {Capacity: capacity, Values: values(max(count, 0) % 4)}}
	}
	return servers
}

// anyAnswer draws what a webhook may answer, or fail to.
func anyAnswer() *rapid.Generator[Webhook] {
	review := func(response *autoscalingv1.FleetAutoscaleResponse) *autoscalingv1.FleetAutoscaleReview {
		return &autoscalingv1.FleetAutoscaleReview{Response: response}
	}
	return rapid.SampledFrom([]Webhook{
		nil,
		answering(nil, errors.New("no route to host")),
		answering(nil, nil),
		answering(review(nil), nil),
		answering(review(&autoscalingv1.FleetAutoscaleResponse{Scale: false, Replicas: -1}), nil),
		answering(review(&autoscalingv1.FleetAutoscaleResponse{Scale: true, Replicas: -1}), nil),
		answering(review(&autoscalingv1.FleetAutoscaleResponse{Scale: true, Replicas: 3}), nil),
	})
}

// The controller writes the answer to the Fleet as it is. So whatever is
// stored, and admission does not look at everything, Desired must either
// have a size a Fleet can have or say that it has none. A panic would take
// the control plane down with it, on every pass.
func TestDesiredAnswersWithASizeOrAnErrorWhateverThePolicyAndTheFleet(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		policy, fleet, servers, call := anyPolicy(2).Draw(t, "policy"), anyFleet(t), someServers(t), anyAnswer().Draw(t, "webhook")

		replicas, limited, applied, err := Desired(policy, fleet, servers, nil, noon, call)

		if err != nil {
			assert.Zero(t, replicas, "an error comes with no size")
			assert.False(t, limited)
			assert.Empty(t, applied)
			return
		}
		assert.GreaterOrEqual(t, replicas, int32(0))
		assert.NotEmpty(t, applied, "an answer names the policy that gave it")
	})
}

// A Chain is read from the top: the first entry that has an answer gives
// it, and the ones below are a fallback. An entry further down must never
// win over one that applies.
func TestChainGivesTheAnswerOfTheFirstEntryThatHasOne(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		policies := rapid.SliceOfN(anyPolicy(1), 0, 5).Draw(t, "entries")
		entries := make([]autoscalingv1.ChainEntry, len(policies))
		for i, policy := range policies {
			entries[i] = entry(fmt.Sprintf("entry-%d", i), policy)
		}
		fleet, call := anyFleet(t), anyAnswer().Draw(t, "webhook")

		replicas, limited, applied, err := Desired(chainPolicy(entries...), fleet, nil, nil, noon, call)

		for _, e := range entries {
			want, wantLimited, _, failure := Desired(e.FleetAutoscalerPolicy, fleet, nil, nil, noon, call)
			if failure != nil {
				continue // it passes the turn
			}
			require.NoError(t, err, "%s has an answer", e.ID)
			assert.Equal(t, want, replicas)
			assert.Equal(t, wantLimited, limited)
			assert.Equal(t, nameOf(e), applied)
			return
		}
		assert.Error(t, err, "no entry has an answer")
	})
}

// daily is a Schedule that begins every day at the same time of day.
type daily struct {
	hour, minute int
	zone         string
	length       time.Duration // zero for no end
	from, until  time.Time     // zero for no limit on that side
}

func aDailySchedule(t *rapid.T) daily {
	moments := rapid.SampledFrom([]time.Time{{}, noon.Add(-36 * time.Hour), noon.Add(-time.Minute), noon.Add(time.Minute), noon.Add(36 * time.Hour)})
	return daily{
		hour:   rapid.IntRange(0, 23).Draw(t, "hour"),
		minute: rapid.IntRange(0, 59).Draw(t, "minute"),
		// None of these changes its clock in the week around noon.
		zone:   rapid.SampledFrom([]string{"UTC", "Asia/Tokyo", "America/New_York"}).Draw(t, "zone"),
		length: time.Duration(rapid.SampledFrom([]int{0, 1, 59, 60, 61, 24 * 60, 50 * 60}).Draw(t, "minutes")) * time.Minute,
		from:   moments.Draw(t, "from"),
		until:  moments.Draw(t, "until"),
	}
}

func (d daily) policy() autoscalingv1.SchedulePolicy {
	s := autoscalingv1.SchedulePolicy{}
	s.Between.Start, s.Between.End = metav1.NewTime(d.from), metav1.NewTime(d.until)
	s.ActivePeriod = autoscalingv1.ActivePeriod{Timezone: d.zone, StartCron: fmt.Sprintf("%d %d * * *", d.minute, d.hour)}
	if d.length > 0 {
		s.ActivePeriod.Duration = d.length.String()
	}
	return s
}

// covers says whether the schedule is on at now, by looking at the days
// around now one by one.
func (d daily) covers(now time.Time) bool {
	if now.Before(d.from) || (!d.until.IsZero() && now.After(d.until)) {
		return false
	}
	if d.length == 0 {
		return true // once begun, it never ends
	}
	zone, _ := time.LoadLocation(d.zone)
	for day := -4; day <= 4; day++ {
		begin := time.Date(now.Year(), now.Month(), now.Day()+day, d.hour, d.minute, 0, 0, zone)
		if now.After(begin.Add(-earlyStart)) && now.Before(begin.Add(d.length)) {
			return true
		}
	}
	return false
}

// A Schedule is how an operator says "more servers for the weekend, and
// only then". Outside its time its policy must not be heard, and inside
// its time it must.
func TestAScheduleAppliesInItsTimeAndNeverOutsideIt(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		schedule := aDailySchedule(t)
		now := noon.Add(time.Duration(rapid.IntRange(-48*3600, 48*3600).Draw(t, "seconds")) * time.Second)
		policy := schedulePolicy(schedule.policy(), bufferPolicy(amount(1), 3, 10))

		replicas, _, _, err := Desired(policy, fleetOf(), nil, nil, now, nil)

		if schedule.covers(now) {
			require.NoError(t, err, "at %s", now)
			assert.Equal(t, int32(3), replicas)
			return
		}
		assert.ErrorIs(t, err, ErrInactiveSchedule, "at %s", now)
	})
}

// crew is a Fleet with a Counter or a List, the GameServers that add up to
// its status, and a policy on that Counter or List that upstream's
// validation accepts.
type crew struct {
	fleet   *agonesv1.Fleet
	servers []*agonesv1.GameServer
	policy  autoscalingv1.FleetAutoscalerPolicy
	// What the policy goes by.
	buffer             intstr.IntOrString
	smallest, largest  int64 // the limits on the total capacity
	perReplica, total  usage
	allocated, current int64
	of                 func(*agonesv1.GameServer) usage
}

func aCrew(t *rapid.T) crew {
	c := crew{perReplica: aUsage(t), current: int64(rapid.IntRange(1, 8).Draw(t, "servers"))}
	with, fleetOf, policyOf := counting, counterFleet, counterPolicy
	c.of = func(gs *agonesv1.GameServer) usage {
		return usage{gs.Status.Counters[rooms].Count, gs.Status.Counters[rooms].Capacity}
	}
	if rapid.Bool().Draw(t, "asAList") {
		with, fleetOf, policyOf = listing, listFleet, listPolicy
		c.of = func(gs *agonesv1.GameServer) usage {
			return usage{int64(len(gs.Status.Lists[players].Values)), gs.Status.Lists[players].Capacity}
		}
	}
	for i := range c.current {
		own, state := aUsage(t), rapid.SampledFrom([]agonesv1.GameServerState{ready, ready, allocated}).Draw(t, "state")
		c.servers = append(c.servers, server(fmt.Sprintf("gs-%d", i), state, on("n1"), with(own.count, own.capacity)))
		c.total = usage{c.total.count + own.count, c.total.capacity + own.capacity}
		if state == allocated {
			c.allocated += own.count
		}
	}
	c.buffer, c.smallest, c.largest = someLimits(t)
	c.policy = policyOf(c.buffer, c.smallest, c.largest)
	require.Empty(t, c.policy.ValidatePolicy(field.NewPath("policy")), "upstream should accept it")
	c.fleet = fleetOf(capacityCase{perReplica: c.perReplica, replicas: int32(c.current), total: c.total, allocated: c.allocated})
	return c
}

// aUsage draws a small Counter or List that holds no more than it can.
func aUsage(t *rapid.T) usage {
	capacity := rapid.Int64Range(0, 6).Draw(t, "capacity")
	return usage{count: rapid.Int64Range(0, capacity).Draw(t, "count"), capacity: capacity}
}

// someLimits draws a buffer and limits on the total capacity to go with it.
func someLimits(t *rapid.T) (buffer intstr.IntOrString, smallest, largest int64) {
	least, buffer := int64(1), percent(rapid.IntRange(1, 99).Draw(t, "share"))
	if rapid.Bool().Draw(t, "asANumber") {
		least = rapid.Int64Range(1, 8).Draw(t, "amount")
		buffer = amount(int32(least))
	}
	smallest = rapid.Int64Range(least, least+15).Draw(t, "minCapacity")
	if buffer.Type == intstr.Int && rapid.Bool().Draw(t, "noMinimum") {
		smallest = 0
	}
	return buffer, smallest, rapid.Int64Range(max(smallest, least), 60).Draw(t, "maxCapacity")
}

// keepsBufferFree reports whether that much free capacity is the buffer or
// more. It only multiplies.
func (c crew) keepsBufferFree(free int64) bool {
	if c.buffer.Type == intstr.Int {
		return free >= int64(c.buffer.IntValue())
	}
	share, _ := intstr.GetScaledValueFromIntOrPercent(&c.buffer, 100, true)
	return free*int64(100-share) >= c.allocated*int64(share)
}

// without returns what is left of the total once scaling down has taken
// its first n GameServers.
func (c crew) without(n int64) usage {
	left := c.total
	for _, gs := range (pass{fleet: c.fleet, servers: c.servers}).removalOrder()[:n] {
		own := c.of(gs)
		left = usage{left.count - own.count, left.capacity - own.capacity}
	}
	return left
}

// A Counter or List policy has a buffer of free capacity to keep and
// limits on the total capacity. Its answer may fall short of the buffer
// for the sake of the limits, but it must never make things worse:
// growing must not go past the maximum, and scaling down must neither cut
// into the buffer nor go below the minimum, or to no replica at all.
func TestACapacityPolicyStaysWithinItsLimitsAndKeepsItsBuffer(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		c := aCrew(t)

		answer, _, _, err := Desired(c.policy, c.fleet, c.servers, nil, noon, nil)

		if err != nil {
			return // a new replica would bring nothing: there is no answer
		}
		replicas := int64(answer)
		switch {
		case replicas > c.current && c.total.capacity >= c.smallest:
			grown := c.total.capacity + (replicas-c.current)*c.perReplica.capacity
			assert.LessOrEqual(t, grown, c.largest, "capacity with %d replicas", replicas)
		case replicas < c.current && c.total.capacity <= c.largest && !c.asksForOneReplicaOnly():
			left := c.without(c.current - replicas)
			assert.GreaterOrEqual(t, left.capacity, c.smallest, "capacity with %d replicas", replicas)
			assert.True(t, c.keepsBufferFree(left.free()), "%d free with %d replicas", left.free(), replicas)
		}
		assert.GreaterOrEqual(t, replicas, min(c.current, int64(fewest)))
	})
}

// asksForOneReplicaOnly is upstream's shortcut for a share of nothing: it
// goes down to one replica without looking at the GameServers.
func (c crew) asksForOneReplicaOnly() bool {
	return c.buffer.Type == intstr.String && c.allocated == 0 && c.perReplica.capacity >= c.smallest
}
