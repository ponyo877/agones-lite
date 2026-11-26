package agent

import (
	"testing"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ponyo877/scrim/formal/vectors"
	"github.com/ponyo877/scrim/pkg/docker"
	"github.com/ponyo877/scrim/pkg/gameservers"
	"github.com/ponyo877/scrim/pkg/podspec"
)

// These tests tie the Quint model in formal/quint/agent.qnt to this
// package. The model checker runs an agent that plans by the model's
// tables through every order of crashes and faults; what it finds holds
// for this agent as far as wantOf, conditionOf and planOne are the same
// functions. The tables are written out whole, so passing means they are.

// The names the model has for what Go keeps as numbers.
var (
	quintWants = map[string]want{
		"WantsRemoved": wantsRemoved, "WantsCreated": wantsCreated, "WantsStarted": wantsStarted,
		"WantsRunning": wantsRunning, "WantsKept": wantsKept,
	}
	quintConditions = map[string]condition{
		"Broken": broken, "ExitedCleanlyAlready": exitedCleanly, "ExitedWithErrorAlready": exitedWithError,
		"HalfStarted": halfStarted, "GameRuns": gameRuns, "InBetween": inBetween,
	}
	quintActs = map[string]ActionKind{
		"Create": Create, "Start": Start, "Remove": Remove, "Discard": Discard, "Tell": Tell,
	}
	// What Docker says of a container, by the model's name for it: its
	// state and its exit code. A name that is not here is no container.
	quintSidecars = map[string]docker.Container{
		"SidecarCreated": {State: "created"}, "SidecarRunning": {State: "running"}, "SidecarExited": {State: "exited"},
	}
	quintGames = map[string]docker.Container{
		"GameCreated": {State: "created"}, "GameRunning": {State: "running"}, "GameOdd": {State: "paused"},
		"GameExitedCleanly": {State: "exited"}, "GameExitedWithError": {State: "exited", ExitCode: 1},
	}
)

// quintServer is the GameServer a model speaks of: in a state, with a
// record that is Live or Leaving.
func quintServer(state, stage string) *agonesv1.GameServer {
	gs := server("gs", agonesv1.GameServerState(state))
	if gameservers.Stage(stage) == gameservers.Leaving {
		gs.DeletionTimestamp = &metav1.Time{}
	}
	return gs
}

// quintContainers is the containers a model speaks of. own says whether
// they carry the uid of the GameServer, or that of an earlier one.
func quintContainers(sidecar, game string, own bool) []docker.Container {
	var containers []docker.Container
	if is, there := quintSidecars[sidecar]; there {
		containers = append(containers, container("gs", podspec.RoleSidecar, is.State, is.ExitCode))
	}
	if is, there := quintGames[game]; there {
		containers = append(containers, container("gs", podspec.RoleGame, is.State, is.ExitCode))
	}
	if !own {
		return ofAnEarlier(containers)
	}
	return containers
}

func TestWantOfAgreesWithTheQuintModel(t *testing.T) {
	type vector struct{ State, Stage, Want string }
	all := vectors.Read[vector](t, "quint/wants.jsonl")
	for _, v := range all {
		want, known := quintWants[v.Want]
		require.True(t, known, v.Want)
		assert.Equal(t, want, wantOf(quintServer(v.State, v.Stage)), "%+v", v)
	}
	assert.Len(t, all, 11*2, "every state, Live and Leaving")
}

func TestConditionOfAgreesWithTheQuintModel(t *testing.T) {
	type vector struct{ Sidecar, Game, Condition string }
	all := vectors.Read[vector](t, "quint/conditions.jsonl")
	for _, v := range all {
		condition, known := quintConditions[v.Condition]
		require.True(t, known, v.Condition)
		assert.Equal(t, condition, conditionOf(quintContainers(v.Sidecar, v.Game, true)), "%+v", v)
	}
	assert.Len(t, all, (len(quintSidecars)+1)*(len(quintGames)+1), "every sidecar with every game container")
}

func TestPlanOneAgreesWithTheQuintModel(t *testing.T) {
	type vector struct {
		State, Stage, Sidecar, Game string
		Own                         bool
		Act, Event                  string
	}
	all := vectors.Read[vector](t, "quint/plans.jsonl")
	for _, v := range all {
		action, acts := planOne(quintServer(v.State, v.Stage), quintContainers(v.Sidecar, v.Game, v.Own))

		if v.Act == "Rest" {
			assert.False(t, acts, "%+v", v)
			continue
		}
		kind, known := quintActs[v.Act]
		require.True(t, known, v.Act)
		assert.True(t, acts, "%+v", v)
		assert.Equal(t, kind, action.Kind, "%+v", v)
		if kind == Tell {
			assert.Equal(t, gameservers.Event(v.Event), action.Event, "%+v", v)
		}
	}
	assert.Len(t, all, 11*2*(len(quintSidecars)+1)*(len(quintGames)+1)*2)
}
