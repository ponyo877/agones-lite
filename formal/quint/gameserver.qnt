// -*- mode: Bluespec; -*-

/// The tables a GameServer lives by: three pure functions, each the model
/// of a Go function in pkg/gameservers. `nextState` is Next (state.go),
/// `nextStage` is NextStage and `decide` is decide (lifecycle.go). The
/// vectors module writes down what they answer for every input, and Go
/// tests hold the real functions against that.
module gameserverTables {
  // ---- The table of states (pkg/gameservers/state.go)

  type State =
    | PortAllocation | Creating | Starting | Scheduled | RequestReady | Ready
    | Reserved | Allocated | Shutdown | Unhealthy | Error

  type Event =
    // reported by the control plane itself
    | Placed | Readied | AllocatedByRequest | ScaledDown | NodeLost
    // reported by the node's agent
    | ContainersCreated | CreateFailed | ContainersRunning
    | ExitedCleanly | ExitedWithError | ContainersMissing | ContainersRemoved
    // reported by the sidecar
    | SDKReady | SDKAllocate | SDKReserve | SDKShutdown | HealthFailed | ReserveExpired
    // the events that move the record on, and not the state (lifecycle.go)
    | DeletionAsked | Abandoned
    // what decide answers when there is nothing to report
    | Nothing

  pure val states = Set(
    PortAllocation, Creating, Starting, Scheduled, RequestReady, Ready,
    Reserved, Allocated, Shutdown, Unhealthy, Error
  )

  pure val events = Set(
    Placed, Readied, AllocatedByRequest, ScaledDown, NodeLost,
    ContainersCreated, CreateFailed, ContainersRunning,
    ExitedCleanly, ExitedWithError, ContainersMissing, ContainersRemoved,
    SDKReady, SDKAllocate, SDKReserve, SDKShutdown, HealthFailed, ReserveExpired,
    DeletionAsked, Abandoned
  )

  /// Go: Final. A state a GameServer never leaves.
  pure def final(s: State): bool = Set(Shutdown, Unhealthy, Error).contains(s)

  /// Every state that is not final: the SDK may ask for anything at any time.
  pure val anywhere = states.filter(s => not(final(s)))

  /// Go: beforeSession. No players are on the server, or about to be.
  pure val beforeSession = Set(PortAllocation, Creating, Starting, Scheduled, RequestReady, Ready)

  /// Go: rules. One row of the table: the states the event applies in, and
  /// where it leads. An event without a row applies nowhere.
  pure def rule(e: Event): { from: Set[State], to: State } = match e {
    | Placed => { from: Set(PortAllocation, Creating), to: Creating }
    | ContainersCreated => { from: Set(Creating), to: Starting }
    | CreateFailed => { from: Set(Creating, Starting), to: Error }
    | ContainersRunning => { from: Set(Starting), to: Scheduled }
    | Readied => { from: Set(RequestReady), to: Ready }
    | AllocatedByRequest => { from: Set(Ready, Allocated), to: Allocated }
    | ScaledDown => { from: beforeSession, to: Shutdown }
    | SDKReady => { from: anywhere, to: RequestReady }
    | SDKAllocate => { from: anywhere, to: Allocated }
    | SDKReserve => { from: anywhere, to: Reserved }
    | SDKShutdown => { from: anywhere, to: Shutdown }
    | ReserveExpired => { from: Set(Reserved), to: RequestReady }
    | ExitedCleanly => { from: anywhere, to: Shutdown }
    | ExitedWithError => { from: anywhere, to: Unhealthy }
    | ContainersMissing => { from: anywhere, to: Unhealthy }
    | HealthFailed => { from: anywhere, to: Unhealthy }
    | NodeLost => { from: anywhere, to: Unhealthy }
    | _ => { from: Set(), to: Error }
  }

  /// Go: the ok of Next.
  pure def applies(s: State, e: Event): bool = rule(e).from.contains(s)

  /// Go: Next. The state after an event; the same state when the event
  /// does not apply, so that late and repeated events change nothing.
  pure def nextState(s: State, e: Event): State = if (applies(s, e)) rule(e).to else s

  /// Go: reachable. Players can connect in this state, so the address
  /// must be published by the time it is entered.
  pure def playable(s: State): bool = Set(Scheduled, Ready, Reserved, Allocated).contains(s)

  // ---- The life of the record (pkg/gameservers/lifecycle.go)

  type Stage = Live | Leaving | Gone

  pure val stages = Set(Live, Leaving, Gone)

  /// Go: stageRules.
  pure def stageRule(e: Event): { from: Set[Stage], to: Stage } = match e {
    | DeletionAsked => { from: Set(Live), to: Leaving }
    | ContainersRemoved => { from: Set(Leaving), to: Gone }
    | Abandoned => { from: Set(Leaving), to: Gone }
    | _ => { from: Set(), to: Gone }
  }

  /// Go: the ok of NextStage.
  pure def moves(stage: Stage, e: Event): bool = stageRule(e).from.contains(stage)

  /// Go: NextStage.
  pure def nextStage(stage: Stage, e: Event): Stage = if (moves(stage, e)) stageRule(e).to else stage

  /// Go: decide. The Event the control plane itself has to report next.
  pure def decide(state: State, stage: Stage, placed: bool, reachable: bool): Event =
    if (stage == Leaving and (not(placed) or not(reachable))) Abandoned
    else if (stage == Leaving) Nothing
    else if (state == Shutdown) DeletionAsked
    else if (not(placed) and not(final(state))) Placed
    else if (state == RequestReady) Readied
    else Nothing

  // ---- What the Go tests replay

  pure val transitionVectors = tuples(states, events).map(((s, e)) =>
    { state: s, event: e, next: nextState(s, e), ok: applies(s, e) })

  pure val stageVectors = tuples(Set(Live, Leaving), events).map(((stage, e)) =>
    { stage: stage, event: e, next: nextStage(stage, e), ok: moves(stage, e) })

  pure val decisionVectors =
    tuples(states, Set(Live, Leaving), Set(true, false), Set(true, false)).map(((s, stage, placed, reachable)) =>
      { state: s, stage: stage, placed: placed, reachable: reachable, event: decide(s, stage, placed, reachable) })
}

/// The life of one GameServer, as the control plane keeps it.
///
/// Everybody who can report an Event does so here, in any order: the
/// control plane's own loop, the agent, the sidecar, the allocation API, an
/// operator, and the node coming and going. The model checker tries every
/// order there is.
module gameserver {
  import gameserverTables.*

  // ---- One GameServer, and everybody who has something to say about it

  /// The node the GameServer is placed on, as the control plane knows it.
  type Link = Connected | Disconnected | Deleted

  /// Who reports an Event.
  type Reporter = ControlPlane | Agent | Sidecar | Matchmaker | Operator | Nobody

  type GameServer = {
    state: State,
    stage: Stage,
    placed: bool,   // status.nodeName is set
    address: bool,  // status.address and status.ports are set
  }

  var gs: GameServer
  var node: Link
  /// The step that led here: what the GameServer was, the Event, and who
  /// reported it. It is how the invariants below speak about a step.
  var was: GameServer
  var event: Event
  var by: Reporter

  /// Go: Controller.Report and Controller.moveOn. An Event that does not
  /// apply changes nothing, and neither does one for a record that is on
  /// its way out. An Event that would send players to a node nobody knows
  /// any more is held back too.
  pure def reported(g: GameServer, n: Link, e: Event): GameServer =
    if (moves(g.stage, e)) { ...g, stage: nextStage(g.stage, e) }
    else if (g.stage != Live or not(applies(g.state, e))) g
    else if (playable(nextState(g.state, e)) and not(g.placed and n != Deleted)) g
    else {
      state: nextState(g.state, e),
      stage: g.stage,
      placed: g.placed or e == Placed,
      address: g.address or playable(nextState(g.state, e)),
    }

  action report(who: Reporter, e: Event): bool = all {
    gs' = reported(gs, node, e),
    node' = node,
    was' = gs,
    event' = e,
    by' = who,
  }

  /// A GameServer starts in one of two states: PortAllocation when it has
  /// a Dynamic or Passthrough port, Creating otherwise.
  action init = all {
    nondet first = oneOf(Set(PortAllocation, Creating))
    gs' = { state: first, stage: Live, placed: false, address: false },
    node' = Connected,
    was' = { state: PortAllocation, stage: Live, placed: false, address: false },
    event' = Nothing,
    by' = Nobody,
  }

  /// Go: Controller.sync. The loop reports what decide chose. A node is
  /// only chosen while one is connected.
  action sync = {
    val chosen = decide(gs.state, gs.stage, gs.placed, node == Connected)
    all {
      gs.stage != Gone,
      chosen != Nothing,
      chosen == Placed implies node == Connected,
      report(ControlPlane, chosen),
    }
  }

  pure val agentEvents = Set(
    ContainersCreated, CreateFailed, ContainersRunning,
    ExitedCleanly, ExitedWithError, ContainersMissing, ContainersRemoved
  )

  pure val sidecarEvents = Set(SDKReady, SDKAllocate, SDKReserve, SDKShutdown, HealthFailed, ReserveExpired)

  /// The agent reports anything about a GameServer placed on its node, in
  /// or out of turn: it repeats itself, and reports of an earlier pass
  /// arrive late.
  action agentReports = all {
    gs.stage != Gone, gs.placed, node == Connected,
    nondet e = oneOf(agentEvents)
    report(Agent, e),
  }

  /// So does the sidecar: a game server calls the SDK when it likes.
  action sidecarReports = all {
    gs.stage != Gone, gs.placed, node == Connected,
    nondet e = oneOf(sidecarEvents)
    report(Sidecar, e),
  }

  /// Go: gameserverallocations. A request is given a GameServer on a node
  /// players can be sent to.
  action allocation = all {
    gs.stage == Live, gs.placed, node == Connected,
    report(Matchmaker, AllocatedByRequest),
  }

  /// Go: gameserversets (too many replicas) and nodes (a node being drained).
  action scaleDown = all {
    gs.stage == Live,
    report(ControlPlane, ScaledDown),
  }

  /// Go: nodes.Controller. A node that was deleted, or that has been out
  /// of reach for too long, takes its GameServers with it.
  action nodeLost = all {
    gs.stage != Gone, gs.placed, node != Connected,
    report(ControlPlane, NodeLost),
  }

  /// An operator deletes the GameServer, or the Fleet it belongs to.
  action deletion = all {
    gs.stage != Gone,
    report(Operator, DeletionAsked),
  }

  /// The node falls silent, comes back, is deleted, or registers again.
  action nodeChanges = all {
    nondet link = oneOf(Set(Connected, Disconnected, Deleted))
    all { link != node, node' = link },
    gs' = gs, was' = gs, event' = Nothing, by' = Nobody,
  }

  action step = any {
    sync, agentReports, sidecarReports, allocation, scaleDown, nodeLost, deletion, nodeChanges,
  }

  // ---- What must hold after every step

  /// A final state is final.
  val finalIsFinal = final(was.state) implies gs.state == was.state

  /// Once a record is on its way out, nothing moves its state any more,
  /// and nothing brings it back.
  val leavingIsOneWay = and {
    was.stage == Leaving implies (gs.stage != Live and gs.state == was.state),
    was.stage == Gone implies gs == was,
  }

  /// The game server's own requests and failures are what ends a session.
  /// Nothing the control plane does by routine takes players off a server,
  /// and neither does a matchmaker.
  pure val endsASession = Set(
    SDKReady, SDKReserve, SDKShutdown,
    ExitedCleanly, ExitedWithError, ContainersMissing, HealthFailed, NodeLost
  )
  val sessionsEndForAReason = {
    val hadSession = was.state == Allocated and was.stage == Live
    val hasSession = gs.state == Allocated and gs.stage == Live
    (hadSession and not(hasSession)) implies or {
      endsASession.contains(event),
      event == DeletionAsked and by == Operator,
    }
  }

  /// Wherever players can be sent, the address was published. A game
  /// server may ask for a session before its agent reports it running.
  val playableHasAnAddress = playable(gs.state) implies gs.address

  /// A GameServer without a node has no containers, so it is in one of the
  /// states that need none.
  val unplacedNeedsNoContainers =
    not(gs.placed) implies Set(PortAllocation, Creating, Shutdown).contains(gs.state)

  /// What the control plane decides to report always applies: its loop
  /// never asks for the same thing for ever.
  val decisionsApply = {
    val chosen = decide(gs.state, gs.stage, gs.placed, node == Connected)
    gs.stage == Gone or chosen == Nothing or applies(gs.state, chosen) or moves(gs.stage, chosen)
  }

  /// The same for every input there is, reachable or not.
  pure val everyDecisionApplies =
    tuples(states, Set(Live, Leaving), Set(true, false), Set(true, false)).forall(((s, stage, placed, reachable)) => {
      val chosen = decide(s, stage, placed, reachable)
      or {
        chosen == Nothing, applies(s, chosen), moves(stage, chosen),
        // The one gap: a state that needs containers, on a GameServer
        // without a node. unplacedNeedsNoContainers says it cannot come up.
        not(placed) and not(Set(PortAllocation, Creating).contains(s)),
      }
    })

  val safe = and {
    finalIsFinal, leavingIsOneWay, sessionsEndForAReason, playableHasAnAddress,
    unplacedNeedsNoContainers, decisionsApply, everyDecisionApplies,
  }

  // ---- What must come about in the end

  /// The agent removes the containers of a GameServer that is leaving.
  action agentCleansUp = all {
    gs.stage == Leaving, gs.placed, node == Connected,
    report(Agent, ContainersRemoved),
  }

  /// A deletion that was asked for is carried out, whatever the node does:
  /// its agent reports the cleanup, or the control plane stops waiting.
  temporal deletionsEnd =
    (strongFair(sync, (gs, node, was, event, by)) and strongFair(agentCleansUp, (gs, node, was, event, by)))
      implies always(gs.stage == Leaving implies eventually(gs.stage == Gone))

  /// A game server that shut down is removed without anybody asking.
  temporal shutdownsAreCleanedUp =
    (strongFair(sync, (gs, node, was, event, by)) and strongFair(agentCleansUp, (gs, node, was, event, by)))
      implies always(gs.state == Shutdown implies eventually(gs.stage == Gone))

  // ---- Lives worth reading

  /// The ordinary life, from creation to the end of a session.
  run ordinaryLifeTest =
    init
      .then(sync)
      .expect(gs.state == Creating and gs.placed)
      .then(report(Agent, ContainersCreated))
      .then(report(Agent, ContainersRunning))
      .expect(gs.state == Scheduled and gs.address)
      .then(report(Sidecar, SDKReady))
      .then(sync)
      .expect(gs.state == Ready)
      .then(allocation)
      .expect(gs.state == Allocated)
      .then(report(Sidecar, SDKShutdown))
      .then(sync)
      .expect(gs.stage == Leaving)
      .then(agentCleansUp)
      .expect(gs.stage == Gone)

  /// A game server may allocate itself before its agent reports it running.
  run allocatedBeforeRunningTest =
    init
      .then(sync)
      .then(report(Agent, ContainersCreated))
      .then(report(Sidecar, SDKAllocate))
      .expect(gs.state == Allocated and gs.address)
      .then(report(Agent, ContainersRunning))
      .expect(gs.state == Allocated)

  /// A matchmaker is not given a GameServer that is still coming up.
  run nothingToAllocateYetTest =
    init
      .then(sync)
      .then(report(Agent, ContainersCreated))
      .then(allocation)
      .expect(gs.state == Starting)

  /// A deleted GameServer whose node is out of reach is dropped at once.
  run unreachableIsAbandonedTest =
    init
      .then(sync)
      .then(report(Operator, DeletionAsked))
      .then(all { node' = Disconnected, gs' = gs, was' = gs, event' = Nothing, by' = Nobody })
      .then(sync)
      .expect(gs.stage == Gone and event == Abandoned)
}
