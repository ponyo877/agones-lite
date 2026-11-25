package fleetautoscalers

import (
	"errors"
	"fmt"
	"math"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	autoscalingv1 "agones.dev/agones/pkg/apis/autoscaling/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/ponyo877/scrim/pkg/gameserversets"
)

// usage is a Counter or a List as two numbers.
type usage struct {
	count    int64 // a Counter's count, or how many values a List holds
	capacity int64
}

// free is the capacity that is not in use. Upstream calls it the available
// capacity.
func (u usage) free() int64 { return u.capacity - u.count }

// capacityPolicy is a Counter or a List policy together with the numbers it
// works on. The two policies differ only in where those numbers come from.
type capacityPolicy struct {
	bufferSize intstr.IntOrString // free capacity to keep: an amount, or a share in percent
	min, max   int64              // limits on the Fleet's total capacity

	perReplica usage // what a new GameServer starts with: the Fleet's template
	total      usage // the sum over the Fleet's GameServers: the Fleet's status
	allocated  int64 // the part of total.count that is on Allocated GameServers

	// of reads the Counter or List off one GameServer. ok is false when the
	// GameServer does not have it.
	of func(gs *agonesv1.GameServer) (u usage, ok bool)
}

// fewest is as far as a Counter or List policy scales down: upstream cannot
// take these Fleets to zero yet.
const fewest int32 = 1

// counter keeps part of a Counter's capacity free across the Fleet.
func (p pass) counter(c *autoscalingv1.CounterPolicy) (int32, bool, error) {
	if c == nil {
		return 0, false, errors.New("counterPolicy parameter must not be nil")
	}
	template, ok := p.fleet.Spec.Template.Spec.Counters[c.Key]
	if !ok {
		return 0, false, fmt.Errorf("cannot apply CounterPolicy as Counter key %s does not exist in the Fleet Spec", c.Key)
	}
	sum, ok := p.fleet.Status.Counters[c.Key]
	if !ok {
		return 0, false, fmt.Errorf("cannot apply CounterPolicy as Counter key %s does not exist in the Fleet Status", c.Key)
	}
	policy := capacityPolicy{
		bufferSize: c.BufferSize, min: c.MinCapacity, max: c.MaxCapacity,
		perReplica: usage{count: template.Count, capacity: template.Capacity},
		total:      usage{count: sum.Count, capacity: sum.Capacity},
		allocated:  sum.AllocatedCount,
		of: func(gs *agonesv1.GameServer) (usage, bool) {
			own, ok := gs.Status.Counters[c.Key]
			return usage{count: own.Count, capacity: own.Capacity}, ok
		},
	}
	return policy.replicas(p.fleet.Status.Replicas, p.removalOrder())
}

// list keeps part of a List's capacity free across the Fleet.
func (p pass) list(l *autoscalingv1.ListPolicy) (int32, bool, error) {
	if l == nil {
		return 0, false, errors.New("listPolicy parameter must not be nil")
	}
	template, ok := p.fleet.Spec.Template.Spec.Lists[l.Key]
	if !ok {
		return 0, false, fmt.Errorf("cannot apply ListPolicy as List key %s does not exist in the Fleet Spec", l.Key)
	}
	sum, ok := p.fleet.Status.Lists[l.Key]
	if !ok {
		return 0, false, fmt.Errorf("cannot apply ListPolicy as List key %s does not exist in the Fleet Status", l.Key)
	}
	policy := capacityPolicy{
		bufferSize: l.BufferSize, min: l.MinCapacity, max: l.MaxCapacity,
		perReplica: usage{count: int64(len(template.Values)), capacity: template.Capacity},
		total:      usage{count: sum.Count, capacity: sum.Capacity},
		allocated:  sum.AllocatedCount,
		of: func(gs *agonesv1.GameServer) (usage, bool) {
			own, ok := gs.Status.Lists[l.Key]
			return usage{count: int64(len(own.Values)), capacity: own.Capacity}, ok
		},
	}
	return policy.replicas(p.fleet.Status.Replicas, p.removalOrder())
}

// removalOrder lists the GameServers that scaling down may take, the first
// to go first. Allocated and Reserved GameServers are not among them. Nor
// are the ones that are going already: the Fleet's status has stopped
// counting those, so neither a replica nor any capacity is left to take off
// for them.
//
// Upstream: getSortedDeletableGameServers, which keeps the ones that are
// going, and so takes them off a second time.
func (p pass) removalOrder() []*agonesv1.GameServer {
	var deletable []*agonesv1.GameServer
	for _, gs := range p.servers {
		if gs.IsDeletable() && !gs.IsBeingDeleted() {
			deletable = append(deletable, gs)
		}
	}
	return gameserversets.SortForRemoval(p.fleet.Spec.Scheduling, deletable, p.counts, p.fleet.Spec.Priorities)
}

// replicas returns the size at which the free capacity matches the buffer,
// as far as the limits on the total capacity allow. removable is what
// scaling down would take, in that order.
//
// Upstream: applyCounterOrListPolicy.
func (c capacityPolicy) replicas(current int32, removable []*agonesv1.GameServer) (int32, bool, error) {
	buffer, err := bufferFor(c.allocated, c.bufferSize)
	if err != nil {
		return 0, false, err
	}
	if c.bufferSize.Type == intstr.String && c.allocated <= 0 && c.perReplica.capacity >= c.min {
		// A share of nothing is nothing: only the minimum holds the Fleet
		// up, and one replica is enough for that.
		return fewest, true, nil
	}
	free := c.total.free()
	switch {
	case c.total.capacity < c.min:
		return c.growToMin(current)
	case c.total.capacity > c.max && free <= buffer:
		// With more free than the buffer, the last case does the shrinking.
		return c.shrinkToMax(current, removable)
	case free < buffer:
		return c.grow(current, buffer-free)
	case free > buffer:
		return c.shrink(current, removable, buffer)
	}
	return current, false, nil
}

// allows reports whether the total capacity stays within the policy's limits
// with extra more replicas.
func (c capacityPolicy) allows(extra int64) bool {
	room := c.max - c.total.capacity
	if c.perReplica.capacity > 0 && extra > room/c.perReplica.capacity {
		// Over the maximum. Dividing says so without working out the
		// capacity, which may be more than an int64 holds.
		return false
	}
	capacity := c.total.capacity + extra*c.perReplica.capacity
	return capacity >= c.min && capacity <= c.max
}

// growToMin adds the replicas that bring the total capacity up to the
// minimum.
//
// Upstream: scaleUpLimited. It only refuses a capacity of 0, and would add
// replicas forever for a negative one.
func (c capacityPolicy) growToMin(replicas int32) (int32, bool, error) {
	if c.perReplica.capacity <= 0 {
		return 0, false, errors.New("cannot scale up as Capacity is equal to 0")
	}
	missing := c.min - c.total.capacity
	return plus(replicas, divideUp(missing, c.perReplica.capacity)), true, nil
}

// grow adds the replicas that make up for the missing free capacity, or as
// many of them as fit under the maximum.
//
// Upstream: scaleUp.
func (c capacityPolicy) grow(replicas int32, missing int64) (int32, bool, error) {
	gain := c.perReplica.free()
	if gain <= 0 {
		return 0, false, errors.New("cannot scale up as adding additional replicas does not increase available Capacity")
	}
	extra := divideUp(missing, gain)
	if c.allows(extra) {
		return plus(replicas, extra), false, nil
	}
	fit := (c.max - c.total.capacity) / c.perReplica.capacity
	return plus(replicas, fit), true, nil
}

// shrinkToMax removes GameServers until the total capacity is down to the
// maximum.
//
// Upstream: scaleDownLimited.
func (c capacityPolicy) shrinkToMax(replicas int32, removable []*agonesv1.GameServer) (int32, bool, error) {
	capacity := c.total.capacity
	for _, gs := range removable {
		if capacity <= c.max {
			break
		}
		own, _ := c.of(gs)
		capacity -= own.capacity
		replicas--
	}
	return max(replicas, fewest), true, nil
}

// shrink removes GameServers for as long as the buffer stays free and the
// total capacity stays at or above the minimum, down to one replica.
//
// Upstream: scaleDown. It holds on to one replica only when it runs out of
// GameServers, and can stop earlier with none, or with fewer than none when
// the Fleet's status is behind its GameServers.
func (c capacityPolicy) shrink(replicas int32, removable []*agonesv1.GameServer, buffer int64) (int32, bool, error) {
	if c.total.capacity == c.min {
		return replicas, true, nil
	}
	replicas, limited := c.without(replicas, removable, buffer)
	return max(replicas, fewest), limited, nil
}

// without takes GameServers off replicas until one more would cut into the
// buffer or into the minimum capacity. Every GameServer may hold a different
// amount, so they are taken one at a time. limited says that the minimum is
// what stopped it.
//
// Upstream: the loop of scaleDown. A GameServer without the Counter or List
// is removed too, there as here.
func (c capacityPolicy) without(replicas int32, removable []*agonesv1.GameServer, buffer int64) (left int32, limited bool) {
	rest := c.total
	for _, gs := range removable {
		replicas--
		own, ok := c.of(gs)
		if !ok {
			continue
		}
		rest = usage{count: rest.count - own.count, capacity: rest.capacity - own.capacity}
		switch free := rest.free(); {
		case free < buffer:
			return replicas + 1, false // this one stays, for the buffer
		case rest.capacity < c.min:
			return replicas + 1, true // this one stays, for the minimum
		case free == buffer:
			return replicas, false
		case rest.capacity == c.min:
			return replicas, true
		}
	}
	return replicas, false
}

// plus is replicas and extra more, as the size of a Fleet.
func plus(replicas int32, extra int64) int32 {
	return sizeOf(int64(replicas) + int64(sizeOf(extra)))
}

// sizeOf is n as the size of a Fleet. A number too large for one is the
// largest size there is: converted as it is, it would wrap around, and could
// come out below zero.
func sizeOf(n int64) int32 {
	return int32(min(max(n, math.MinInt32), math.MaxInt32))
}
