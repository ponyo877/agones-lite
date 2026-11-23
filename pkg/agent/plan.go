// Package agent runs on each node and makes Docker match what the control
// plane wants there.
//
// Start with plan.go. Plan compares the GameServers placed on this node with
// the containers that exist and returns what to do about each. It is a pure
// function; agent.go carries its answers out, and sidecars.go serves the
// sidecars of the running game servers.
//
// Upstream counterpart: none. In Agones this is the kubelet's job.
package agent

import (
	"sort"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/ponyo877/agones-lite/pkg/docker"
	"github.com/ponyo877/agones-lite/pkg/gameservers"
	"github.com/ponyo877/agones-lite/pkg/podspec"
)

// Key names a GameServer.
type Key = types.NamespacedName

// ActionKind is what the agent does about one GameServer.
type ActionKind int

// The five things the agent can do.
const (
	Create  ActionKind = iota // pull images, create and start the containers
	Start                     // start containers that were created and left at that
	Remove                    // stop and delete the containers, then report ContainersRemoved
	Discard                   // delete containers that belong to nothing we know
	Tell                      // report an Event and touch nothing
)

// Action is one decision of Plan.
type Action struct {
	Kind       ActionKind
	Key        Key
	GameServer *agonesv1.GameServer // set for Create and Remove
	Containers []docker.Container   // set for Start, Remove and Discard
	Event      gameservers.Event    // set for Tell
}

// Plan decides what to do so that the containers match the GameServers
// placed on this node. It returns at most one Action per GameServer.
func Plan(desired []*agonesv1.GameServer, actual []docker.Container) []Action {
	containers := group(actual)
	var actions []Action
	for _, gs := range desired {
		key := Key{Namespace: gs.Namespace, Name: gs.Name}
		if action, ok := planOne(gs, containers[key]); ok {
			action.Key = key
			actions = append(actions, action)
		}
		delete(containers, key)
	}
	return append(actions, discardRest(containers)...)
}

// planOne handles one GameServer and the containers that carry its name.
func planOne(gs *agonesv1.GameServer, containers []docker.Container) (Action, bool) {
	creating := gs.Status.State == agonesv1.GameServerStateCreating
	switch {
	case gs.DeletionTimestamp != nil:
		return Action{Kind: Remove, GameServer: gs, Containers: containers}, true
	case creating && len(containers) == 0:
		return Action{Kind: Create, GameServer: gs}, true
	case creating:
		// Left over from a creation that was interrupted. Clear them and
		// start again on the next pass.
		return Action{Kind: Discard, Containers: containers}, true
	case gameservers.Final(gs.Status.State):
		return Action{}, false // kept as they are until the GameServer is deleted
	}
	if event, ok := trouble(containers); ok {
		return Action{Kind: Tell, Event: event}, true
	}
	if gs.Status.State == agonesv1.GameServerStateStarting {
		return resume(containers)
	}
	return Action{}, false
}

// trouble turns containers that are gone or have stopped into an Event.
func trouble(containers []docker.Container) (gameservers.Event, bool) {
	sidecar, hasSidecar := withRole(containers, podspec.RoleSidecar)
	game, hasGame := withRole(containers, podspec.RoleGame)
	switch {
	case !hasSidecar || !hasGame || sidecar.Exited():
		return gameservers.ContainersMissing, true
	case game.Exited() && game.ExitCode == 0:
		return gameservers.ExitedCleanly, true
	case game.Exited():
		return gameservers.ExitedWithError, true
	}
	return "", false
}

// resume takes up a start that was cut short. The agent may have stopped
// between creating the containers and starting them all, or its report
// that they run may have been lost.
func resume(containers []docker.Container) (Action, bool) {
	if waiting := neverStarted(containers); len(waiting) > 0 {
		return Action{Kind: Start, Containers: waiting}, true
	}
	game, _ := withRole(containers, podspec.RoleGame)
	return Action{Kind: Tell, Event: gameservers.ContainersRunning}, game.State == "running"
}

// neverStarted returns the containers that were created and left at that,
// the sidecar first: the others join its network and cannot start before it.
func neverStarted(containers []docker.Container) []docker.Container {
	var sidecar, others []docker.Container
	for _, c := range containers {
		switch {
		case c.State != "created":
		case c.Labels[podspec.LabelRole] == podspec.RoleSidecar:
			sidecar = append(sidecar, c)
		default:
			others = append(others, c)
		}
	}
	return append(sidecar, others...)
}

// discardRest removes containers whose GameServer is not on this node any more.
func discardRest(containers map[Key][]docker.Container) []Action {
	var actions []Action
	for key, group := range containers {
		actions = append(actions, Action{Kind: Discard, Key: key, Containers: group})
	}
	sort.Slice(actions, func(i, j int) bool { return actions[i].Key.String() < actions[j].Key.String() })
	return actions
}

func group(containers []docker.Container) map[Key][]docker.Container {
	out := map[Key][]docker.Container{}
	for _, c := range containers {
		key := Key{Namespace: c.Labels[podspec.LabelNamespace], Name: c.Labels[podspec.LabelGameServer]}
		out[key] = append(out[key], c)
	}
	return out
}

func withRole(containers []docker.Container, role string) (docker.Container, bool) {
	for _, c := range containers {
		if c.Labels[podspec.LabelRole] == role {
			return c, true
		}
	}
	return docker.Container{}, false
}
