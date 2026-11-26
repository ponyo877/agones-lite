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

	"github.com/ponyo877/scrim/pkg/docker"
	"github.com/ponyo877/scrim/pkg/gameservers"
	"github.com/ponyo877/scrim/pkg/podspec"
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
	Kind ActionKind
	Key  Key
	// UID is the uid of the GameServer the Action is about, and goes into
	// what is reported of it. Containers that belong to no GameServer have
	// none.
	UID        types.UID
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
			action.Key, action.UID = key, gs.UID
			actions = append(actions, action)
		}
		delete(containers, key)
	}
	return append(actions, discardRest(containers)...)
}

// want is what a GameServer asks of its containers: how far bringing them
// up should have come, going by its state and by the Stage of its record.
type want int

const (
	wantsRemoved want = iota // Leaving: stop and delete them
	wantsCreated             // Creating: the control plane knows of none yet
	wantsStarted             // Starting: it knows they were made, and waits for them to run
	wantsRunning             // any later state that is not final: they all ran, and should still
	wantsKept                // a final state: as they are, until the GameServer is deleted
)

func wantOf(gs *agonesv1.GameServer) want {
	switch state := gs.Status.State; {
	case gameservers.StageOf(gs) == gameservers.Leaving:
		return wantsRemoved
	case state == agonesv1.GameServerStateCreating:
		return wantsCreated
	case gameservers.Final(state):
		return wantsKept
	case state == agonesv1.GameServerStateStarting:
		return wantsStarted
	}
	return wantsRunning
}

// planOne handles one GameServer and the containers that carry its name.
func planOne(gs *agonesv1.GameServer, containers []docker.Container) (Action, bool) {
	switch wanted := wantOf(gs); wanted {
	case wantsRemoved:
		return Action{Kind: Remove, GameServer: gs, Containers: containers}, true
	case wantsCreated:
		return create(gs, containers), true
	case wantsKept:
		return Action{}, false
	default:
		return watchOver(wanted, containers)
	}
}

// create handles a GameServer the control plane takes to have no containers.
func create(gs *agonesv1.GameServer, containers []docker.Container) Action {
	switch {
	case len(containers) == 0:
		return Action{Kind: Create, GameServer: gs}
	case started(gs, containers):
		// It is gs that is out of date, not the containers: see started.
		// Saying it again is ignored by a control plane that knows.
		return tell(gameservers.ContainersCreated)
	}
	// Left over from a creation that was interrupted, or from an earlier
	// GameServer of the same name. Clear them and start again on the next
	// pass.
	return Action{Kind: Discard, Containers: containers}
}

// started reports whether a container of gs itself, and not of an earlier
// GameServer of the same name, was ever started.
//
// The agent starts containers only once the control plane has taken
// ContainersCreated. So when gs is Creating all the same, gs is behind. An
// agent on another machine sees a copy of its GameServers that arrives
// after its own reports are answered: a little later on every start.
func started(gs *agonesv1.GameServer, containers []docker.Container) bool {
	for _, c := range containers {
		if c.Labels[podspec.LabelUID] == string(gs.UID) && c.State != "created" {
			return true
		}
	}
	return false
}

// condition is what the containers of one GameServer amount to, once the
// control plane has taken note of them.
type condition int

const (
	broken          condition = iota // the sidecar or the game container is missing, or the sidecar has stopped
	exitedCleanly                    // the game container ended with exit code 0
	exitedWithError                  // the game container ended with any other code
	halfStarted                      // some were created and left at that
	gameRuns                         // all were started, and the game container runs
	inBetween                        // all were started; the game container neither runs nor has ended
)

// conditionOf looks at the containers. Of the conditions that fit, the
// first one counts: containers that have stopped are not started again.
func conditionOf(containers []docker.Container) condition {
	sidecar, hasSidecar := withRole(containers, podspec.RoleSidecar)
	game, hasGame := withRole(containers, podspec.RoleGame)
	switch {
	case !hasSidecar || !hasGame || sidecar.Exited():
		return broken
	case game.Exited() && game.ExitCode == 0:
		return exitedCleanly
	case game.Exited():
		return exitedWithError
	case len(neverStarted(containers)) > 0:
		return halfStarted
	case game.State == "running":
		return gameRuns
	}
	return inBetween
}

// reactions is what the agent does about containers in a condition, by what
// the GameServer asks of them. A pair that is not listed calls for nothing.
var reactions = map[want]map[condition]Action{
	wantsStarted: {
		broken:          tell(gameservers.ContainersMissing),
		exitedCleanly:   tell(gameservers.ExitedCleanly),
		exitedWithError: tell(gameservers.ExitedWithError),
		// A start that was cut short is taken up again. The agent may have
		// stopped between creating the containers and starting them all.
		halfStarted: {Kind: Start},
		// Its report that they run may have been lost.
		gameRuns: tell(gameservers.ContainersRunning),
	},
	wantsRunning: {
		broken:          tell(gameservers.ContainersMissing),
		exitedCleanly:   tell(gameservers.ExitedCleanly),
		exitedWithError: tell(gameservers.ExitedWithError),
		// Past Starting, all of its containers ran. Ones that never did are
		// not those: they were made later, by an agent that did not know
		// better, and what the game server ran in is gone. Nothing would
		// ever start them, and the GameServer would wait for ever.
		halfStarted: tell(gameservers.ContainersMissing),
	},
}

// tell is the Action that reports event and touches nothing.
func tell(event gameservers.Event) Action {
	return Action{Kind: Tell, Event: event}
}

// watchOver handles a GameServer whose containers the control plane has
// taken note of: it does what their condition calls for.
func watchOver(wanted want, containers []docker.Container) (Action, bool) {
	action, called := reactions[wanted][conditionOf(containers)]
	if action.Kind == Start {
		action.Containers = neverStarted(containers)
	}
	return action, called
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
