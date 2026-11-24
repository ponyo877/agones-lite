package fleets

import (
	"fmt"
	"slices"
	"testing"

	"agones.dev/agones/pkg/apis"
	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"pgregory.net/rapid"
)

// These tests play a rollout through in a small model of the rest of the
// system and let rapid look for a Fleet, and a course of events, that break
// what a rollout promises. A failure is printed as the smallest such story.

// squad is a GameServerSet together with the GameServers it really has.
type squad struct {
	set      *agonesv1.GameServerSet
	starting int32 // on their way to Ready
	ready    int32
	inUse    int32 // Allocated: a session is on them
	leaving  int32 // shutting down
}

func (s *squad) alive() int32 { return s.starting + s.ready + s.inUse }
func (s *squad) empty() bool  { return s.alive()+s.leaving == 0 }

// readyAtItsSize is how many of its Ready GameServers stay once the set
// has taken the size it is asked for now.
func (s *squad) readyAtItsSize() int32 {
	resized := *s
	resized.resize()
	return resized.ready
}

// waitingFor is how many GameServers the set asks for that are not up.
func (s *squad) waitingFor() int32 {
	return s.set.Spec.Replicas - s.ready - s.inUse - s.leaving
}

// report writes the status the GameServerSet controller would.
func (s *squad) report() {
	s.set.Status = agonesv1.GameServerSetStatus{
		Replicas: s.alive(), ReadyReplicas: s.ready, AllocatedReplicas: s.inUse, ShutdownReplicas: s.leaving,
	}
}

// resize is the GameServerSet controller's pass: it adds what is missing
// and removes what is too many, never a GameServer in use. Of the others,
// the ones that are not Ready yet go first.
func (s *squad) resize() {
	surplus := s.alive() - s.set.Spec.Replicas
	if surplus < 0 {
		s.starting -= surplus
		return
	}
	unready := min(surplus, s.starting)
	idle := min(surplus-unready, s.ready)
	s.starting, s.ready, s.leaving = s.starting-unready, s.ready-idle, s.leaving+unready+idle
}

// rollout is a Fleet, its squads, and how the story has gone so far.
type rollout struct {
	fleet  *agonesv1.Fleet
	squads []*squad
	made   int // sets created so far, for their names
}

func (r *rollout) sets() []*agonesv1.GameServerSet {
	sets := make([]*agonesv1.GameServerSet, len(r.squads))
	for i, s := range r.squads {
		sets[i] = s.set
	}
	return sets
}

func (r *rollout) squad(name string) *squad {
	i := slices.IndexFunc(r.squads, func(s *squad) bool { return s.set.Name == name })
	return r.squads[i]
}

// total adds up one number over all squads.
func (r *rollout) total(of func(*squad) int32) int32 {
	var sum int32
	for _, s := range r.squads {
		sum += of(s)
	}
	return sum
}

func asked(s *squad) int32 { return s.set.Spec.Replicas }

// split returns the squad of the Fleet's current template, an empty one
// when there is none yet, and the squads of earlier templates.
func (r *rollout) split() (current *squad, earlier []*squad) {
	active, rest := Active(r.fleet, r.sets())
	current = &squad{set: r.fleet.GameServerSet()}
	if active != nil {
		current = r.squad(active.Name)
	}
	for _, set := range rest {
		earlier = append(earlier, r.squad(set.Name))
	}
	return current, earlier
}

// sum adds up one number over some squads.
func sum(squads []*squad, of func(*squad) int32) int32 {
	var total int32
	for _, s := range squads {
		total += of(s)
	}
	return total
}

func sessions(s *squad) int32 { return s.inUse }

// pass is one pass of the Fleet controller: it plans, carries the plan
// out, and stores the status it saw.
func (r *rollout) pass(t *rapid.T) Changes {
	sets := r.sets()
	status := Status(r.fleet, sets)
	changes, err := Plan(r.fleet, sets)
	require.NoError(t, err)
	r.carryOut(changes)
	r.fleet.Status = status
	return changes
}

func (r *rollout) carryOut(changes Changes) {
	if set := changes.Create; set != nil {
		r.made++
		set.Name = fmt.Sprintf("new-%d", r.made)
		born(set, 10+r.made)
		r.squads = append(r.squads, &squad{set: set})
	}
	if set := changes.Update; set != nil {
		r.squad(set.Name).set = set
	}
	for name, replicas := range changes.Scale {
		r.squad(name).set.Spec.Replicas = replicas
	}
	// Like the controller, which deletes a set only once it is empty.
	r.squads = slices.DeleteFunc(r.squads, func(s *squad) bool {
		return slices.Contains(changes.Delete, s.set.Name) && s.empty()
	})
}

// moveOn is what the rest of the system does between two passes of the
// Fleet controller: the sets take their new sizes, GameServers that were
// shutting down are gone, and events decide what happens to the others.
func (r *rollout) moveOn(events course) {
	for _, s := range r.squads {
		s.resize()
		s.leaving = events.stillLeaving(s.leaving)
		up := events.gotReady(s.starting)
		taken := events.gotAllocated(s.ready + up)
		over := events.sessionsOver(s.inUse)
		s.starting, s.ready, s.inUse, s.leaving = s.starting-up, s.ready+up-taken, s.inUse+taken-over, s.leaving+over
		s.report()
	}
}

// course is a course of events: how many of n GameServers each thing
// happens to.
type course struct {
	stillLeaving func(n int32) int32
	gotReady     func(n int32) int32
	gotAllocated func(n int32) int32
	sessionsOver func(n int32) int32
}

func all(n int32) int32  { return n }
func none(n int32) int32 { return 0 }

// anything lets rapid decide everything, except that no session ends: a
// session that ended would take its GameServer away, and that is not the
// rollout's doing.
func anything(t *rapid.T) course {
	some := func(label string) func(int32) int32 {
		return func(n int32) int32 { return rapid.Int32Range(0, n).Draw(t, label) }
	}
	return course{stillLeaving: some("stillLeaving"), gotReady: some("gotReady"), gotAllocated: some("gotAllocated"), sessionsOver: none}
}

// calm is the course of events when all goes well and nobody plays.
var calm = course{stillLeaving: none, gotReady: all, gotAllocated: none, sessionsOver: none}

// aRollout draws a Fleet in the middle of things: up to three sets of
// earlier templates and perhaps one of the current, none of them
// necessarily at its size.
func aRollout(t *rapid.T, strategy appsv1.DeploymentStrategyType) *rollout {
	fleet := &agonesv1.Fleet{}
	fleet.Namespace, fleet.Name, fleet.UID = "default", "fleet", "1234"
	fleet.Spec.Replicas = rapid.Int32Range(0, 12).Draw(t, "replicas")
	fleet.Spec.Scheduling = apis.Packed
	fleet.Spec.Strategy.Type = strategy
	fleet.ApplyDefaults()
	if strategy == appsv1.RollingUpdateDeploymentStrategyType {
		rollingBy(fleet, aStep().Draw(t, "maxSurge"), aStep().Draw(t, "maxUnavailable"))
	}
	r := &rollout{fleet: fleet}
	for i := range rapid.IntRange(0, 3).Draw(t, "oldSets") {
		r.squads = append(r.squads, aSquad(t, old(fleet, fmt.Sprintf("old-%d", i), shape{})))
	}
	if rapid.Bool().Draw(t, "hasActive") {
		r.squads = append(r.squads, aSquad(t, current(fleet, "active", shape{})))
	}
	fleet.Status = Status(fleet, r.sets())
	return r
}

// aStep draws a maxSurge or maxUnavailable that admission lets through.
func aStep() *rapid.Generator[string] {
	return rapid.SampledFrom([]string{"1", "2", "3", "20", "1%", "25%", "50%", "99%"})
}

func aSquad(t *rapid.T, set *agonesv1.GameServerSet) *squad {
	few := rapid.Int32Range(0, 4)
	s := &squad{
		set:      born(set, rapid.IntRange(0, 2).Draw(t, "born")),
		starting: few.Draw(t, "starting"),
		ready:    rapid.Int32Range(0, 8).Draw(t, "ready"),
		inUse:    few.Draw(t, "inUse"),
		leaving:  few.Draw(t, "leaving"),
	}
	s.set.Spec.Replicas = rapid.Int32Range(0, 12).Draw(t, "asked")
	s.report()
	return s
}

// step is what one maxSurge or maxUnavailable comes to for a Fleet of the
// given size, read the way Kubernetes reads a Deployment's.
func step(t *rapid.T, value *intstr.IntOrString, replicas int32, roundUp bool) int32 {
	n, err := intstr.GetScaledValueFromIntOrPercent(value, int(replicas), roundUp)
	require.NoError(t, err)
	return int32(n)
}

// A rollout must not ask for more GameServers than the Fleet's size and
// maxSurge allow: they cost money, and a node may not have the room. A
// Fleet that is over already, say because it was made smaller, must at
// least not grow. Sessions are not asked for: they stay until they end,
// whatever size their set has, and come on top.
func TestARolloutNeverAsksForMoreThanTheSizeAndTheSurge(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		r := aRollout(t, appsv1.RollingUpdateDeploymentStrategyType)
		limit := r.fleet.Spec.Replicas + step(t, r.fleet.Spec.Strategy.RollingUpdate.MaxSurge, r.fleet.Spec.Replicas, true)

		for range rapid.IntRange(1, 12).Draw(t, "passes") {
			before := r.total(asked)
			r.pass(t)

			require.LessOrEqual(t, r.total(asked), max(limit, before), "asked of all sets: %s", specs(r.sets()))
			r.moveOn(anything(t))
		}
	})
}

// spare is how many Ready GameServers a pass may take from the old sets,
// the way upstream reads maxUnavailable: as a share of the GameServers that
// were Ready at the last pass, at least one and at most the Fleet's size.
// What is Ready now, less what that leaves, is spare.
func (r *rollout) spare(t *rapid.T) int32 {
	seen := r.fleet.Status.ReadyReplicas
	unavailable := max(step(t, r.fleet.Spec.Strategy.RollingUpdate.MaxUnavailable, seen, false), 1)
	floor := min(seen-min(unavailable, r.fleet.Spec.Replicas), r.fleet.Spec.Replicas)
	return r.total(func(s *squad) int32 { return s.ready }) - floor
}

// A rollout must leave players something to play on. Each pass has a
// number of Ready GameServers it may take from the old sets, and it must
// take no more. While the new set is still waiting for that many
// GameServers or more, it must take none: the new ones have to be up first.
//
// The floor this is measured from is upstream's, and it follows the Ready
// GameServers down when new ones do not come up: see
// a_version_that_never_gets_ready_takes_most_of_the_fleet in
// formal/Formal/Rollout.lean.
func TestARolloutTakesNoMoreReadyGameServersFromTheOldSetsThanAreSpare(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		r := aRollout(t, appsv1.RollingUpdateDeploymentStrategyType)

		for range rapid.IntRange(1, 12).Draw(t, "passes") {
			current, earlier := r.split()
			spare, waiting := r.spare(t), current.waitingFor()
			// What an earlier pass gave up already is not this one's doing.
			before, askedBefore := sum(earlier, (*squad).readyAtItsSize), sum(earlier, asked)
			r.pass(t)

			taken := before - sum(earlier, (*squad).readyAtItsSize)
			require.LessOrEqual(t, taken, max(spare, 0), "Ready GameServers taken, with %d waited for: %s", waiting, specs(r.sets()))
			if waiting >= spare {
				require.Equal(t, askedBefore, sum(earlier, asked), "asked of the old sets, with %d spare and %d waited for", spare, waiting)
			}
			r.moveOn(anything(t))
		}
	})
}

// The surge holds the new set back. It must not take from it what it has:
// those are the GameServers the rollout is for. The new set only gets
// smaller to leave room for sessions on old GameServers, which count
// toward the Fleet's size until they end.
func TestARolloutOnlyMakesTheNewSetSmallerToLeaveRoomForSessions(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		r := aRollout(t, aStrategy().Draw(t, "strategy"))

		for range rapid.IntRange(1, 12).Draw(t, "passes") {
			current, earlier := r.split()
			room := max(r.fleet.Spec.Replicas-sum(earlier, sessions), 0)
			before := current.set.Spec.Replicas
			r.pass(t)

			current, _ = r.split()
			require.GreaterOrEqual(t, current.set.Spec.Replicas, min(before, room), "asked of the new set: %s", specs(r.sets()))
			r.moveOn(anything(t))
		}
	})
}

// A set that still has GameServers takes them along when it is deleted,
// sessions and all.
func TestARolloutOnlyDeletesSetsThatAreEmpty(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		r := aRollout(t, aStrategy().Draw(t, "strategy"))

		for range rapid.IntRange(1, 12).Draw(t, "passes") {
			before := slices.Clone(r.squads)
			changes := r.pass(t)

			for _, s := range before {
				if slices.Contains(changes.Delete, s.set.Name) {
					require.True(t, s.empty(), "%s is deleted with %d GameServers, %d of them in use", s.set.Name, s.alive()+s.leaving, s.inUse)
				}
			}
			r.moveOn(anything(t))
		}
	})
}

func aStrategy() *rapid.Generator[appsv1.DeploymentStrategyType] {
	return rapid.SampledFrom([]appsv1.DeploymentStrategyType{appsv1.RollingUpdateDeploymentStrategyType, appsv1.RecreateDeploymentStrategyType})
}

// However a rollout began and whatever happened on the way, once things
// calm down it must come to an end: one set of the current template at the
// Fleet's size, less the sessions that are still running on GameServers of
// earlier templates, and those GameServers are all that is left of them.
// It must then stay there.
func TestARolloutComesToAnEndAndStaysThere(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		r := aRollout(t, aStrategy().Draw(t, "strategy"))
		for range rapid.IntRange(0, 6).Draw(t, "passes") {
			r.pass(t)
			r.moveOn(anything(t))
		}
		// Every GameServer changes over in a pass of its own at worst, and
		// each such pass needs another one to see the new GameServer Ready.
		patience := 2*int(r.total((*squad).alive)+r.fleet.Spec.Replicas) + 10

		for range patience {
			r.pass(t)
			r.moveOn(calm)
		}

		current, earlier := r.split()
		require.NotEmpty(t, current.set.Name, "a set of the current template")
		assert.Equal(t, max(r.fleet.Spec.Replicas-sum(earlier, sessions), current.inUse), current.alive(), "GameServers of the current template")
		assert.Zero(t, current.starting+current.leaving, "GameServers of the current template that are not up")
		for _, s := range earlier {
			assert.Equal(t, s.inUse, s.alive()+s.leaving, "%s has more than its sessions left", s.set.Name)
		}
		assert.Equal(t, Changes{}, r.pass(t), "the plan after the end")
	})
}

// Recreate takes the old GameServers away at once and for good: the point
// of it is that two versions never run side by side for longer than it
// takes the old one to stop.
func TestRecreateEmptiesEveryOldSetInOnePassAndKeepsItEmpty(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		r := aRollout(t, appsv1.RecreateDeploymentStrategyType)

		for range rapid.IntRange(1, 6).Draw(t, "passes") {
			r.pass(t)

			active, rest := Active(r.fleet, r.sets())
			require.NotNil(t, active)
			for _, set := range rest {
				require.Zero(t, set.Spec.Replicas, "asked of %s", set.Name)
			}
			assert.LessOrEqual(t, active.Spec.Replicas, r.fleet.Spec.Replicas)
			r.moveOn(anything(t))
		}
	})
}

// A size below zero is no size at all: the store would keep it, and the
// set's controller would remove every GameServer it can.
func TestPlanNeverAsksForASizeBelowZero(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		r := aRollout(t, aStrategy().Draw(t, "strategy"))

		for range rapid.IntRange(1, 12).Draw(t, "passes") {
			r.pass(t)

			for _, set := range r.sets() {
				require.GreaterOrEqual(t, set.Spec.Replicas, int32(0), "asked of %s", set.Name)
			}
			r.moveOn(anything(t))
		}
	})
}

// The store hands sets over in no particular order, and two passes over
// the same Fleet must come to the same plan.
func TestPlanGivesTheSameAnswerForAnyOrderOfTheSets(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		r := aRollout(t, aStrategy().Draw(t, "strategy"))
		sets := r.sets()
		want, err := Plan(r.fleet, sets)
		require.NoError(t, err)

		got, err := Plan(r.fleet, rapid.Permutation(sets).Draw(t, "shuffled"))

		require.NoError(t, err)
		assert.Equal(t, want, got)
	})
}
