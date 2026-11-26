package gameservers

import (
	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
)

// Stage is where the record of a GameServer stands between being wanted and
// being dropped. The state says what the game server is doing; the Stage
// says whether anybody still wants it to.
//
// A deletion takes two steps, which is the job a finalizer does in
// Kubernetes: the record is marked, and it is dropped once its containers
// are gone.
type Stage string

// The Stages, in the order a record goes through them.
const (
	// Live means wanted: Events move its state by the table in state.go.
	Live Stage = "Live"
	// Leaving means its deletion was asked for. Nothing moves its state any
	// more, and its containers are being removed.
	Leaving Stage = "Leaving"
	// Gone means the record is dropped. Nothing leads back from here: a
	// GameServer made later under the same name is another one.
	Gone Stage = "Gone"
)

// The Events that move a record on. They leave the state alone, so the
// table in state.go has no row for them. The third is ContainersRemoved,
// which the agent reports.
const (
	// DeletionAsked comes from an operator, from the GameServerSet the
	// GameServer belongs to, or from the control plane once it is Shutdown.
	DeletionAsked Event = "DeletionAsked"
	// Abandoned is the control plane's own: nobody will report the
	// containers removed, because the GameServer never had a node or its
	// node cannot be reached.
	Abandoned Event = "Abandoned"
)

// stageRules is the life of a record: one row for each Event that moves it.
var stageRules = map[Event]struct{ from, to Stage }{
	DeletionAsked:     {from: Live, to: Leaving},
	ContainersRemoved: {from: Leaving, to: Gone},
	Abandoned:         {from: Leaving, to: Gone},
}

// NextStage returns the Stage a record moves to when an Event arrives. ok is
// false when the event does not apply: a deletion asked for twice, or a
// cleanup reported for a GameServer nobody wanted removed.
func NextStage(current Stage, e Event) (next Stage, ok bool) {
	r, known := stageRules[e]
	if !known || r.from != current {
		return current, false
	}
	return r.to, true
}

// StageOf reads the Stage of a GameServer the store holds, which is never
// Gone.
func StageOf(gs *agonesv1.GameServer) Stage {
	if gs.DeletionTimestamp != nil {
		return Leaving
	}
	return Live
}

// nothing is what decide answers when the control plane has no Event of its
// own to report.
const nothing Event = ""

// decide is the judgement half of the sync loop: the Event the control
// plane itself has to report next about gs. It looks only at gs and at
// whether the agent of its node can currently be reached.
func decide(gs *agonesv1.GameServer, reachable bool) Event {
	unplaced := gs.Status.NodeName == ""
	state := gs.Status.State
	switch {
	case StageOf(gs) == Leaving && (unplaced || !reachable):
		// Nobody will report the cleanup. If the node comes back, its agent
		// finds containers that belong to nothing and removes them.
		return Abandoned
	case StageOf(gs) == Leaving:
		return nothing // the agent reports ContainersRemoved when it is done
	case state == agonesv1.GameServerStateShutdown:
		return DeletionAsked
	case unplaced && !Final(state):
		return Placed
	case state == agonesv1.GameServerStateRequestReady:
		return Readied
	}
	return nothing
}
