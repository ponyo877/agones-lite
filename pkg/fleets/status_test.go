package fleets

import (
	"math"
	"testing"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	"github.com/stretchr/testify/assert"
)

// Upstream: TestControllerUpdateFleetStatus.
func TestStatusAddsUpTheSets(t *testing.T) {
	sets := []*agonesv1.GameServerSet{
		{Status: agonesv1.GameServerSetStatus{Replicas: 3, ReadyReplicas: 2, ReservedReplicas: 4, AllocatedReplicas: 1, ShutdownReplicas: 6}},
		{Status: agonesv1.GameServerSetStatus{Replicas: 5, ReadyReplicas: 5, ReservedReplicas: 3, AllocatedReplicas: 2}},
	}

	status := Status(&agonesv1.Fleet{}, sets)

	assert.Equal(t, agonesv1.FleetStatus{
		Replicas:          8,
		ReadyReplicas:     7,
		ReservedReplicas:  7,
		AllocatedReplicas: 3,
		Counters:          map[string]agonesv1.AggregatedCounterStatus{},
		Lists:             map[string]agonesv1.AggregatedListStatus{},
	}, status)
}

func TestStatusStartsFromNothingButTheAllocationCount(t *testing.T) {
	fleet := &agonesv1.Fleet{Status: agonesv1.FleetStatus{
		Replicas:      9,
		ReadyReplicas: 9,
		Allocations:   12,
		Counters:      map[string]agonesv1.AggregatedCounterStatus{"rooms": {Count: 1, Capacity: 2}},
		Lists:         map[string]agonesv1.AggregatedListStatus{"players": {Count: 1, Capacity: 2}},
	}}

	status := Status(fleet, nil)

	assert.Equal(t, agonesv1.FleetStatus{
		Allocations: 12,
		Counters:    map[string]agonesv1.AggregatedCounterStatus{},
		Lists:       map[string]agonesv1.AggregatedListStatus{},
	}, status)
}

// Upstream: TestControllerUpdateFleetCounterStatus.
func TestStatusAddsUpCountersByName(t *testing.T) {
	full := agonesv1.AggregatedCounterStatus{
		AllocatedCount: math.MaxInt64, AllocatedCapacity: math.MaxInt64, Count: math.MaxInt64, Capacity: math.MaxInt64,
	}
	sets := []*agonesv1.GameServerSet{
		{Status: agonesv1.GameServerSetStatus{Counters: map[string]agonesv1.AggregatedCounterStatus{
			"full":    full,
			"another": {AllocatedCount: 11, AllocatedCapacity: 20, Count: 42, Capacity: 100},
		}}},
		{Status: agonesv1.GameServerSetStatus{Counters: map[string]agonesv1.AggregatedCounterStatus{
			"full":    {AllocatedCount: 100, AllocatedCapacity: 100, Count: 100, Capacity: 100},
			"another": {Capacity: 100},
			"third":   {AllocatedCount: 21, AllocatedCapacity: 30, Count: 21, Capacity: 400},
		}}},
	}

	status := Status(&agonesv1.Fleet{}, sets)

	assert.Equal(t, map[string]agonesv1.AggregatedCounterStatus{
		"full":    full, // a sum stops at the largest int64
		"another": {AllocatedCount: 11, AllocatedCapacity: 20, Count: 42, Capacity: 200},
		"third":   {AllocatedCount: 21, AllocatedCapacity: 30, Count: 21, Capacity: 400},
	}, status.Counters)
}

// Upstream: TestControllerUpdateFleetListStatus.
func TestStatusAddsUpListsByName(t *testing.T) {
	sets := []*agonesv1.GameServerSet{
		{Status: agonesv1.GameServerSetStatus{Lists: map[string]agonesv1.AggregatedListStatus{
			"full":    {AllocatedCount: 1000, AllocatedCapacity: 1000, Count: 1000, Capacity: 1000},
			"another": {AllocatedCount: 11, AllocatedCapacity: 100, Count: 11, Capacity: 100},
			"third":   {AllocatedCount: 1, AllocatedCapacity: 20, Count: 4, Capacity: 30},
		}}},
		{Status: agonesv1.GameServerSetStatus{Lists: map[string]agonesv1.AggregatedListStatus{
			"full":    {AllocatedCount: 200, AllocatedCapacity: 200, Count: 200, Capacity: 200},
			"another": {AllocatedCount: 1, AllocatedCapacity: 10, Count: 11, Capacity: 100},
		}}},
	}

	status := Status(&agonesv1.Fleet{}, sets)

	assert.Equal(t, map[string]agonesv1.AggregatedListStatus{
		"full":    {AllocatedCount: 1200, AllocatedCapacity: 1200, Count: 1200, Capacity: 1200},
		"another": {AllocatedCount: 12, AllocatedCapacity: 110, Count: 22, Capacity: 200},
		"third":   {AllocatedCount: 1, AllocatedCapacity: 20, Count: 4, Capacity: 30},
	}, status.Lists)
}
