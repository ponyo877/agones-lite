package agent

import (
	"testing"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ponyo877/agones-lite/pkg/docker"
	"github.com/ponyo877/agones-lite/pkg/gameservers"
	"github.com/ponyo877/agones-lite/pkg/podspec"
)

func server(name string, state agonesv1.GameServerState) *agonesv1.GameServer {
	gs := &agonesv1.GameServer{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name}}
	gs.Status.State = state
	return gs
}

func container(gs, role, state string, exitCode int) docker.Container {
	return docker.Container{
		ID:       gs + "-" + role,
		State:    state,
		ExitCode: exitCode,
		Labels: map[string]string{
			podspec.LabelNamespace:  "default",
			podspec.LabelGameServer: gs,
			podspec.LabelRole:       role,
		},
	}
}

func running(gs string) []docker.Container {
	return []docker.Container{
		container(gs, podspec.RoleSidecar, "running", 0),
		container(gs, podspec.RoleGame, "running", 0),
	}
}

func TestPlanForOneGameServer(t *testing.T) {
	gameExited := func(code int) []docker.Container {
		return []docker.Container{
			container("gs", podspec.RoleSidecar, "running", 0),
			container("gs", podspec.RoleGame, "exited", code),
		}
	}
	tests := map[string]struct {
		state      agonesv1.GameServerState
		containers []docker.Container
		kind       ActionKind
		event      gameservers.Event
		nothing    bool
	}{
		"placed, nothing exists yet":         {state: agonesv1.GameServerStateCreating, kind: Create},
		"placed, leftovers from a crash":     {state: agonesv1.GameServerStateCreating, containers: running("gs"), kind: Discard},
		"starting and the game runs":         {state: agonesv1.GameServerStateStarting, containers: running("gs"), kind: Tell, event: gameservers.ContainersRunning},
		"ready and running":                  {state: agonesv1.GameServerStateReady, containers: running("gs"), nothing: true},
		"ready but the containers are gone":  {state: agonesv1.GameServerStateReady, kind: Tell, event: gameservers.ContainersMissing},
		"the game exited cleanly":            {state: agonesv1.GameServerStateAllocated, containers: gameExited(0), kind: Tell, event: gameservers.ExitedCleanly},
		"the game crashed":                   {state: agonesv1.GameServerStateAllocated, containers: gameExited(2), kind: Tell, event: gameservers.ExitedWithError},
		"unhealthy is left for inspection":   {state: agonesv1.GameServerStateUnhealthy, containers: gameExited(2), nothing: true},
		"shutdown waits for the delete mark": {state: agonesv1.GameServerStateShutdown, containers: running("gs"), nothing: true},
		"the sidecar died": {
			state: agonesv1.GameServerStateReady,
			containers: []docker.Container{
				container("gs", podspec.RoleSidecar, "exited", 1),
				container("gs", podspec.RoleGame, "running", 0),
			},
			kind: Tell, event: gameservers.ContainersMissing,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			actions := Plan([]*agonesv1.GameServer{server("gs", tc.state)}, tc.containers)
			if tc.nothing {
				assert.Empty(t, actions)
				return
			}
			require.Len(t, actions, 1)
			assert.Equal(t, tc.kind, actions[0].Kind)
			assert.Equal(t, tc.event, actions[0].Event)
			assert.Equal(t, Key{Namespace: "default", Name: "gs"}, actions[0].Key)
		})
	}
}

func TestPlanRemovesWhatIsBeingDeleted(t *testing.T) {
	gs := server("gs", agonesv1.GameServerStateShutdown)
	now := metav1.Now()
	gs.DeletionTimestamp = &now

	actions := Plan([]*agonesv1.GameServer{gs}, running("gs"))

	require.Len(t, actions, 1)
	assert.Equal(t, Remove, actions[0].Kind)
	assert.Len(t, actions[0].Containers, 2)

	// With nothing left to remove the agent still has to report it.
	actions = Plan([]*agonesv1.GameServer{gs}, nil)
	require.Len(t, actions, 1)
	assert.Equal(t, Remove, actions[0].Kind)
}

func TestPlanDiscardsContainersNobodyWants(t *testing.T) {
	desired := []*agonesv1.GameServer{server("kept", agonesv1.GameServerStateReady)}
	actual := append(running("kept"), running("orphan")...)

	actions := Plan(desired, actual)

	require.Len(t, actions, 1)
	assert.Equal(t, Discard, actions[0].Kind)
	assert.Equal(t, "orphan", actions[0].Key.Name)
	assert.Len(t, actions[0].Containers, 2)
}
