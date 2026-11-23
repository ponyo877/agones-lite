package fleets

import (
	"cmp"
	"slices"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
)

// Changes is what Plan wants done to a Fleet's GameServerSets. The zero value
// means there is nothing to do. No set is named in two fields, so the order
// they are carried out in does not matter.
type Changes struct {
	// Create is a new active set, from fleet.GameServerSet(), when no existing
	// set matches the Fleet's current template. Nil otherwise.
	Create *agonesv1.GameServerSet
	// Update is a copy of the active set with the Fleet's scheduling and
	// priorities, and with its new spec.replicas. Nil when the active set
	// already has both: a new size alone goes in Scale.
	Update *agonesv1.GameServerSet
	// Scale is the new spec.replicas by GameServerSet name, for the sets
	// whose size changes.
	Scale map[string]int32
	// Delete names the old sets that are empty and can be removed.
	Delete []string
}

// Plan decides what to do with the GameServerSets of a Fleet.
//
// fleet.Status must be what the previous pass stored, not a fresh sum: a
// rolling update compares the Ready GameServers of then and now.
//
// It fails only for a Fleet that admission would have refused: an unknown
// strategy, or a maxSurge or maxUnavailable that cannot be read. Upstream
// gives up on the pass in the same places.
//
// Upstream: syncFleet and upsertGameServerSet in pkg/fleets/controller.go.
func Plan(fleet *agonesv1.Fleet, sets []*agonesv1.GameServerSet) (Changes, error) {
	if !fleet.DeletionTimestamp.IsZero() {
		return Changes{}, nil // a Fleet on its way out is not kept in shape
	}
	active, rest := Active(fleet, sets)
	isNew := active == nil
	if isNew {
		active = fleet.GameServerSet()
	}
	replicas, scale, err := resize(fleet, active, rest)
	if err != nil {
		return Changes{}, err
	}
	changes := Changes{Delete: emptied(rest)}
	for _, name := range changes.Delete {
		delete(scale, name) // upstream resizes such a set and then deletes it
	}
	switch {
	case isNew:
		active.Spec.Replicas = replicas
		changes.Create = active
	case !upToDate(active, fleet):
		changes.Update = updated(active, fleet, replicas)
	case replicas != active.Spec.Replicas:
		scale[active.Name] = replicas
	}
	if len(scale) > 0 {
		changes.Scale = scale
	}
	return changes, nil
}

// Active splits sets into the one that matches the Fleet's current template
// and the rest. active is nil when none matches. When several match, the
// oldest is active and the others join the rest, to be scaled down and
// removed like any old set.
//
// The rest comes oldest first, which is the order it gives up GameServers
// in. Neither answer depends on the order of sets: the store hands them
// over in no particular order.
//
// Upstream: filterGameServerSetByActive in pkg/fleets/controller.go, which
// keeps the rest in the order it was listed in, and of two equally old
// matches takes the one listed first.
func Active(fleet *agonesv1.Fleet, sets []*agonesv1.GameServerSet) (active *agonesv1.GameServerSet, rest []*agonesv1.GameServerSet) {
	for _, set := range oldestFirst(sets) {
		if active == nil && apiequality.Semantic.DeepEqual(set.Spec.Template, fleet.Spec.Template) {
			active = set
			continue
		}
		rest = append(rest, set)
	}
	return active, rest
}

// oldestFirst returns sets by age. Sets of the same age go by name.
func oldestFirst(sets []*agonesv1.GameServerSet) []*agonesv1.GameServerSet {
	sorted := slices.Clone(sets)
	slices.SortFunc(sorted, func(a, b *agonesv1.GameServerSet) int {
		return cmp.Or(
			a.CreationTimestamp.Compare(b.CreationTimestamp.Time),
			cmp.Compare(a.Name, b.Name),
		)
	})
	return sorted
}

// upToDate reports whether the active set has the Fleet's scheduling and
// priorities. Neither is part of the template, so changing them does not
// make a new set.
func upToDate(active *agonesv1.GameServerSet, fleet *agonesv1.Fleet) bool {
	return active.Spec.Scheduling == fleet.Spec.Scheduling &&
		slices.Equal(active.Spec.Priorities, fleet.Spec.Priorities)
}

// updated returns a copy of the active set with what upToDate looks at, and
// with its new size.
func updated(active *agonesv1.GameServerSet, fleet *agonesv1.Fleet, replicas int32) *agonesv1.GameServerSet {
	set := active.DeepCopy()
	set.Spec.Replicas = replicas
	set.Spec.Scheduling = fleet.Spec.Scheduling
	set.Spec.Priorities = slices.Clone(fleet.Spec.Priorities)
	return set
}

// emptied names the sets that have no GameServers left, not even ones that
// are still shutting down.
//
// Upstream: deleteEmptyGameServerSets in pkg/fleets/controller.go.
func emptied(rest []*agonesv1.GameServerSet) []string {
	var names []string
	for _, set := range rest {
		if set.Status.Replicas == 0 && set.Status.ShutdownReplicas == 0 {
			names = append(names, set.Name)
		}
	}
	return names
}
