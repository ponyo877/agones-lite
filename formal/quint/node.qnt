// -*- mode: Bluespec; -*-

/// What an operator asks of a node: the model of Node.Duty, Node.Ask and
/// Node.Schedulable in pkg/apis/scrim/v1/node.go, and of the operations of
/// pkg/nodes/controller.go.
module nodeTables {
  /// Go: Duty.
  type Duty = Open | Cordoned | Draining

  /// Go: NodePhase.
  type Phase = NodeReady | NodeDisconnected

  /// What an operator can do to a node, by the name the API has for it.
  type Operation = Cordon | Drain | Uncordon

  pure val duties = Set(Open, Cordoned, Draining)
  pure val operations = Set(Cordon, Drain, Uncordon)

  /// Go: Node.Duty. Drain decides by itself.
  pure def dutyOf(unschedulable: bool, drain: bool): Duty =
    if (drain) Draining else if (unschedulable) Cordoned else Open

  /// Go: Node.Ask. The two switches of the spec, set the one way that
  /// spells a Duty.
  pure def ask(duty: Duty): { unschedulable: bool, drain: bool } =
    { unschedulable: duty != Open, drain: duty == Draining }

  /// Go: operations. What an operation leaves a node with, whatever was
  /// asked of it before.
  pure def operate(op: Operation): Duty = match op {
    | Cordon => Cordoned
    | Drain => Draining
    | Uncordon => Open
  }

  /// Go: Node.Schedulable.
  pure def schedulable(phase: Phase, duty: Duty): bool = phase == NodeReady and duty == Open

  /// The switches say what was asked, for every Duty: the spec cannot hold
  /// a fourth thing by way of the operations.
  pure val askedIsRead = duties.forall(d => dutyOf(ask(d).unschedulable, ask(d).drain) == d)

  // ---- What the Go tests replay

  pure val dutyVectors = tuples(Set(true, false), Set(true, false)).map(((unschedulable, drain)) =>
    { unschedulable: unschedulable, drain: drain, duty: dutyOf(unschedulable, drain) })

  pure val operationVectors = tuples(operations, Set(true, false), Set(true, false)).map(((op, unschedulable, drain)) =>
    { operation: op, unschedulable: unschedulable, drain: drain,
      after: ask(operate(op)), duty: operate(op) })
}

/// The record of a node on another machine, and what keeps it true.
///
/// Its agent says two things. It registers every few seconds, which makes
/// the record and refreshes it. And it holds a Watch call open, which is the
/// only thing that makes the node count as connected: the end of the last
/// call is how the control plane learns that the agent is gone
/// (pkg/nodes/server.go). An operator cordons, drains and deletes, and the
/// control plane itself restarts and forgets every call it had.
module node {
  import nodeTables.*

  pure val maxTime = 3
  pure val maxCalls = 2

  // -- the store
  var known: bool
  var phase: Phase
  var duty: Duty
  var lastHeard: int      // status.lastHeartbeatTime
  // -- the control plane's memory
  var calls: int          // open Watch calls: Go's Server.streams
  // -- the world
  var now: int
  // -- for the invariants
  var silentSince: int    // when the record last became Disconnected

  /// Go: Server.phase. What the open calls say.
  def said = if (calls > 0) NodeReady else NodeDisconnected

  action unchangedRecord = all { known' = known, phase' = phase, duty' = duty, lastHeard' = lastHeard, silentSince' = silentSince }

  action init = all {
    known' = false, phase' = NodeDisconnected, duty' = Open, lastHeard' = 0,
    calls' = 0, now' = 0, silentSince' = 0,
  }

  action timePasses = all { now < maxTime, now' = now + 1, calls' = calls, unchangedRecord }

  /// Go: Server.Register and Local.Register. The record is made, or
  /// refreshed. A node that was gone and is gone still keeps the time it
  /// was last heard from.
  action registers = all {
    known' = true,
    phase' = said,
    duty' = if (known) duty else Open,
    lastHeard' = if (known and phase == NodeDisconnected and said == NodeDisconnected) lastHeard else now,
    silentSince' = if (said == NodeDisconnected and not(known and phase == NodeDisconnected)) now else silentSince,
    calls' = calls, now' = now,
  }

  /// Go: Server.connected. A Watch call opens or ends, and the record
  /// follows when that changes what the calls say.
  action callChanges(delta: int): bool = {
    val after = if (calls + delta > 0) NodeReady else NodeDisconnected
    val moved = known and phase != after
    all {
      calls + delta >= 0, calls + delta <= maxCalls,
      calls' = calls + delta,
      phase' = if (moved) after else phase,
      lastHeard' = if (moved) now else lastHeard,
      silentSince' = if (moved and after == NodeDisconnected) now else silentSince,
      known' = known, duty' = duty, now' = now,
    }
  }

  /// Go: Controller.Operate.
  action operated = all {
    known,
    nondet op = oneOf(operations)
    duty' = operate(op),
    known' = known, phase' = phase, lastHeard' = lastHeard, silentSince' = silentSince,
    calls' = calls, now' = now,
  }

  /// An operator deletes the node. Its agent, if it is still there, makes
  /// it again with its next registration.
  action deleted = all {
    known,
    known' = false,
    phase' = phase, duty' = duty, lastHeard' = lastHeard, silentSince' = silentSince,
    calls' = calls, now' = now,
  }

  /// Go: Controller.StartOver. The control plane restarts: every call it
  /// had is gone with it, and every node starts out Disconnected. One that
  /// was Disconnected already keeps the time it was last heard from.
  action restarts = all {
    calls' = 0,
    phase' = NodeDisconnected,
    lastHeard' = if (known and phase == NodeReady) now else lastHeard,
    silentSince' = if (known and phase == NodeReady) now else silentSince,
    known' = known, duty' = duty, now' = now,
  }

  action step = any {
    timePasses, registers, callChanges(1), callChanges(-1), operated, deleted, restarts,
  }

  // ---- What must hold after every step

  /// A node is exactly as connected as its Watch calls. Were a Register to
  /// make it Ready with no call open, nothing would ever make it
  /// Disconnected again, and its game servers would never be given up.
  val asConnectedAsItsCalls = known implies (phase == said)

  /// While a node stays Disconnected, the time it was last heard from does
  /// not move: an agent that manages to register but not to connect does
  /// not put off the day its GameServers are given up, and neither does a
  /// control plane that restarts.
  val silenceIsNotPutOff = (known and phase == NodeDisconnected) implies lastHeard == silentSince

  /// New game servers and new sessions only go to a node that is connected
  /// and that no operator has closed.
  val schedulableIsConnectedAndOpen = schedulable(phase, duty) implies (calls > 0 or not(known)) and duty == Open

  val safe = and { asConnectedAsItsCalls, silenceIsNotPutOff, schedulableIsConnectedAndOpen, askedIsRead }

  // ---- Histories worth reading

  /// An agent that dials again may briefly hold two calls. The node stays
  /// Ready until the last one ends.
  run twoCallsTest =
    init
      .then(registers)
      .expect(phase == NodeDisconnected)
      .then(callChanges(1))
      .then(callChanges(1))
      .then(callChanges(-1))
      .expect(phase == NodeReady)
      .then(callChanges(-1))
      .expect(phase == NodeDisconnected)

  /// Registering without watching does not make the node look alive.
  run registeringIsNotEnoughTest =
    init
      .then(registers)
      .then(callChanges(1))
      .then(timePasses)
      .then(callChanges(-1))
      .expect(lastHeard == 1)
      .then(timePasses)
      .then(registers)
      .expect(phase == NodeDisconnected and lastHeard == 1)

  /// Cordoning a node that is being drained stops the drain.
  run cordonStopsADrainTest = assert(operate(Drain) == Draining and operate(Cordon) == Cordoned)
}
