// -*- mode: Bluespec; -*-

/// What the agent does about one GameServer and its containers: the model of
/// planOne in pkg/agent/plan.go, in the same three steps. `wantOf` says what
/// the GameServer asks of its containers, `conditionOf` what the containers
/// amount to, and `plan` what to do when the two are put side by side.
module agentTables {
  import gameserverTables.* from "./gameserver"

  /// Go: want.
  type Want = WantsRemoved | WantsCreated | WantsStarted | WantsRunning | WantsKept

  /// Go: wantOf.
  pure def wantOf(state: State, stage: Stage): Want =
    if (stage == Leaving) WantsRemoved
    else if (state == Creating) WantsCreated
    else if (final(state)) WantsKept
    else if (state == Starting) WantsStarted
    else WantsRunning

  /// What Docker says of the sidecar container, as far as the agent looks.
  type Sidecar = NoSidecar | SidecarCreated | SidecarRunning | SidecarExited

  /// The same of the game container. GameOdd is any state that is neither
  /// "created", "running" nor exited: paused, say.
  type Game = NoGame | GameCreated | GameRunning | GameExitedCleanly | GameExitedWithError | GameOdd

  pure val sidecars = Set(NoSidecar, SidecarCreated, SidecarRunning, SidecarExited)
  pure val games = Set(NoGame, GameCreated, GameRunning, GameExitedCleanly, GameExitedWithError, GameOdd)

  /// Go: condition.
  type Condition = Broken | ExitedCleanlyAlready | ExitedWithErrorAlready | HalfStarted | GameRuns | InBetween

  /// Go: conditionOf. The first condition that fits counts.
  pure def conditionOf(sidecar: Sidecar, game: Game): Condition =
    if (sidecar == NoSidecar or game == NoGame or sidecar == SidecarExited) Broken
    else if (game == GameExitedCleanly) ExitedCleanlyAlready
    else if (game == GameExitedWithError) ExitedWithErrorAlready
    else if (sidecar == SidecarCreated or game == GameCreated) HalfStarted
    else if (game == GameRunning) GameRuns
    else InBetween

  /// Go: ActionKind, and for Tell the Event that goes with it.
  type Act = Create | Start | Remove | Discard | Tell | Rest
  type Action = { act: Act, event: Event }

  pure def just(act: Act): Action = { act: act, event: Nothing }
  pure def tell(e: Event): Action = { act: Tell, event: e }

  /// Go: reactions.
  pure def reaction(want: Want, condition: Condition): Action =
    if (want != WantsStarted and want != WantsRunning) just(Rest)
    else match condition {
      | Broken => tell(ContainersMissing)
      | ExitedCleanlyAlready => tell(ExitedCleanly)
      | ExitedWithErrorAlready => tell(ExitedWithError)
      | HalfStarted => if (want == WantsStarted) just(Start) else tell(ContainersMissing)
      | GameRuns => if (want == WantsStarted) tell(ContainersRunning) else just(Rest)
      | InBetween => just(Rest)
    }

  /// Go: started. A container of the GameServer itself, and not of an
  /// earlier one of its name, was started at some time.
  pure def started(sidecar: Sidecar, game: Game, own: bool): bool =
    own and (Set(SidecarRunning, SidecarExited).contains(sidecar) or
             Set(GameRunning, GameExitedCleanly, GameExitedWithError, GameOdd).contains(game))

  /// Go: planOne. own says whether the containers carry the GameServer's
  /// uid, or that of an earlier GameServer of the name.
  pure def plan(want: Want, sidecar: Sidecar, game: Game, own: bool): Action = match want {
    | WantsRemoved => just(Remove)
    | WantsKept => just(Rest)
    | WantsCreated =>
        if (sidecar == NoSidecar and game == NoGame) just(Create)
        else if (started(sidecar, game, own)) tell(ContainersCreated)
        else just(Discard)
    | _ => reaction(want, conditionOf(sidecar, game))
  }

  // ---- What the Go tests replay

  pure val wantVectors = tuples(states, Set(Live, Leaving)).map(((s, stage)) =>
    { state: s, stage: stage, want: wantOf(s, stage) })

  pure val conditionVectors = tuples(sidecars, games).map(((sidecar, game)) =>
    { sidecar: sidecar, game: game, condition: conditionOf(sidecar, game) })

  pure val planVectors =
    tuples(states, Set(Live, Leaving), sidecars, games, Set(true, false)).map(((s, stage, sidecar, game, own)) => {
      val chosen = plan(wantOf(s, stage), sidecar, game, own)
      { state: s, stage: stage, sidecar: sidecar, game: game, own: own, act: chosen.act, event: chosen.event }
    })
}

/// One name on one node: the GameServer the control plane has under it, the
/// containers Docker has under it, and the agent between the two.
///
/// The agent looks at both sides, plans, and carries its plan out one Docker
/// call at a time (pkg/agent/agent.go). Anything may happen between two
/// calls. The GameServer is deleted and made again, the game server exits,
/// a Docker call fails, a report does not get through, the copy the agent
/// works from falls behind, and the agent itself dies and is started again
/// with nothing in its head.
module agent {
  import gameserverTables.* from "./gameserver"
  import agentTables.*

  /// How often things go wrong in one history.
  pure val maxFaults = 2
  /// How many GameServers the name has in one history.
  pure val maxLives = 2

  /// The GameServer under the name, as the control plane has it. uid 0 is
  /// none.
  type Record = { uid: int, state: State, stage: Stage }

  pure val nobody: Record = { uid: 0, state: Creating, stage: Gone }

  /// What the agent is in the middle of: the Docker calls and reports it
  /// has left to make, in order.
  type Call =
    | CreateSidecar | CreateGame | StartSidecar | StartGame
    | StopGame | RemoveAll | Report(Event)

  // -- the control plane
  var record: Record
  var lives: int
  // -- Docker
  var sidecar: Sidecar
  var game: Game
  var madeFor: int          // the uid the containers were made for
  // -- the agent
  var copy: Record          // what it works from: the record, or what the record was
  var behind: bool          // the copy has stopped following the record
  var todo: List[Call]      // the action in progress, what is left of it
  var about: int            // the uid that action is about
  var creating: bool        // whether it is a Create or a Start: what CreateFailed is said of
  // -- what goes wrong
  var faults: int
  var dockerFailed: Set[int] // the uids for which a Docker call failed
  // -- for the invariants
  var complaint: bool        // CreateFailed was taken for a GameServer Docker did not fail
  var tookDown: bool         // a running container of a GameServer that was doing fine was stopped or removed

  action unchangedAgent = all { todo' = todo, about' = about, creating' = creating }
  action unchangedDocker = all { sidecar' = sidecar, game' = game, madeFor' = madeFor }
  action unchangedFaults = all { faults' = faults, dockerFailed' = dockerFailed }
  action unchangedGhosts = all { complaint' = complaint, tookDown' = tookDown }

  /// The copy follows the record, unless it has stopped.
  def copyOf(now: Record): Record = if (behind) copy else now

  action init = all {
    record' = nobody, lives' = 0,
    sidecar' = NoSidecar, game' = NoGame, madeFor' = 0,
    copy' = nobody, behind' = false, todo' = List(), about' = 0, creating' = false,
    faults' = 0, dockerFailed' = Set(),
    complaint' = false, tookDown' = false,
  }

  // ---- What happens to the GameServer and its containers

  /// A GameServer is placed on the node under the name.
  action placed = {
    val fresh = { uid: lives + 1, state: Creating, stage: Live }
    all {
      record.uid == 0, lives < maxLives,
      record' = fresh, lives' = lives + 1, copy' = copyOf(fresh), behind' = behind,
      unchangedDocker, unchangedAgent, unchangedFaults, unchangedGhosts,
    }
  }

  /// Its deletion is asked for.
  action deleted = {
    val marked = { ...record, stage: Leaving }
    all {
      record.uid != 0, record.stage == Live,
      record' = marked, lives' = lives, copy' = copyOf(marked), behind' = behind,
      unchangedDocker, unchangedAgent, unchangedFaults, unchangedGhosts,
    }
  }

  /// The control plane drops a record that is leaving without waiting for
  /// the agent: it could not reach the node.
  action abandoned = all {
    record.stage == Leaving,
    record' = nobody, lives' = lives, copy' = copyOf(nobody), behind' = behind,
    unchangedDocker, unchangedAgent, unchangedFaults, unchangedGhosts,
  }

  /// The game server exits by itself.
  action exits = all {
    game == GameRunning,
    nondet code = oneOf(Set(GameExitedCleanly, GameExitedWithError))
    game' = code,
    sidecar' = sidecar, madeFor' = madeFor,
    record' = record, lives' = lives, copy' = copy, behind' = behind,
    unchangedAgent, unchangedFaults, unchangedGhosts,
  }

  /// The copy stops following the record, or catches up with it.
  action falls = all {
    not(behind), faults < maxFaults,
    behind' = true, copy' = copy, faults' = faults + 1, dockerFailed' = dockerFailed,
    record' = record, lives' = lives, unchangedDocker, unchangedAgent, unchangedGhosts,
  }
  action catchesUp = all {
    behind,
    behind' = false, copy' = record,
    record' = record, lives' = lives, unchangedDocker, unchangedAgent, unchangedFaults, unchangedGhosts,
  }

  /// The agent dies. Started again, it knows nothing of what it was doing.
  action dies = all {
    faults < maxFaults,
    todo' = List(), about' = 0, creating' = false,
    faults' = faults + 1, dockerFailed' = dockerFailed,
    record' = record, lives' = lives, copy' = copy, behind' = behind,
    unchangedDocker, unchangedGhosts,
  }

  // ---- What the agent does

  /// Go: Agent.reconcile and execute. One pass: nothing is in progress for
  /// the name, so the agent plans, and what it plans is the calls to make.
  action plans = {
    val own = madeFor == copy.uid
    val containers = sidecar != NoSidecar or game != NoGame
    val chosen =
      if (copy.uid == 0) (if (containers) just(Discard) else just(Rest))
      else plan(wantOf(copy.state, copy.stage), sidecar, game, own)
    val steps = match chosen.act {
      | Create => List(CreateSidecar, CreateGame, Report(ContainersCreated), StartSidecar, StartGame, Report(ContainersRunning))
      | Start => List(StartSidecar, StartGame, Report(ContainersRunning))
      | Remove => List(StopGame, RemoveAll, Report(ContainersRemoved))
      | Discard => List(StopGame, RemoveAll)
      | Tell => List(Report(chosen.event))
      | Rest => List()
    }
    all {
      todo.length() == 0, steps.length() > 0,
      todo' = steps, about' = copy.uid, creating' = Set(Create, Start).contains(chosen.act),
      record' = record, lives' = lives, copy' = copy, behind' = behind,
      unchangedDocker, unchangedFaults, unchangedGhosts,
    }
  }

  /// The GameServer the containers belong to is doing fine: it is the one
  /// under the name, nobody asked for it to go, and the control plane has
  /// taken note of its containers.
  val standing = and {
    record.uid != 0, record.uid == madeFor, record.stage == Live,
    record.state != Creating, not(final(record.state)),
  }

  /// Go: Controller.Report, and what the control plane does at once about
  /// a report: a request for Ready is granted, and a GameServer that shut
  /// down is marked for deletion.
  pure def taken(r: Record, uid: int, e: Event): Record =
    if (r.uid == 0 or r.uid != uid) r
    else if (e == ContainersRemoved) (if (r.stage == Leaving) nobody else r)
    else if (r.stage != Live or not(applies(r.state, e))) r
    else if (nextState(r.state, e) == Shutdown) { ...r, state: Shutdown, stage: Leaving }
    else { ...r, state: nextState(r.state, e) }

  /// The next call of the action in progress is made, and goes well.
  action calls = {
    val call = todo.head()
    val rest = todo.tail()
    val after = match call {
      | Report(e) => taken(record, about, e)
      | _ => record
    }
    val said = match call {
      | Report(e) => e
      | _ => Nothing
    }
    all {
      todo.length() > 0,
      sidecar' = match call {
        | CreateSidecar => SidecarCreated
        | StartSidecar => if (sidecar == SidecarCreated) SidecarRunning else sidecar
        | RemoveAll => NoSidecar
        | _ => sidecar
      },
      game' = match call {
        | CreateGame => GameCreated
        | StartGame => if (game == GameCreated) GameRunning else game
        | StopGame => if (game == GameRunning) GameExitedWithError else game
        | RemoveAll => NoGame
        | _ => game
      },
      madeFor' = match call {
        | CreateSidecar => about
        | RemoveAll => 0
        | _ => madeFor
      },
      record' = after, lives' = lives, copy' = copyOf(after), behind' = behind,
      todo' = rest, about' = about, creating' = creating,
      unchangedFaults,
      complaint' = (complaint or (said == CreateFailed and after != record and not(dockerFailed.contains(about)))),
      tookDown' = (tookDown or (standing and game == GameRunning and Set(StopGame, RemoveAll).contains(call))),
    }
  }

  /// Go: orCreateFailed. A Docker call of the action in progress fails. The
  /// action ends there. If it was bringing containers up, the agent says
  /// CreateFailed.
  action dockerFails = {
    val call = todo.head()
    val isDocker = match call { | Report(_) => false | _ => true }
    all {
      todo.length() > 0, isDocker, faults < maxFaults,
      todo' = if (creating) List(Report(CreateFailed)) else List(),
      about' = about, creating' = false,
      faults' = faults + 1, dockerFailed' = dockerFailed.union(Set(about)),
      record' = record, lives' = lives, copy' = copy, behind' = behind,
      unchangedDocker, unchangedGhosts,
    }
  }

  /// A report does not get through. The action ends there: the work
  /// stands, and the next pass finds it.
  action reportIsLost = {
    val isReport = match todo.head() { | Report(_) => true | _ => false }
    all {
      todo.length() > 0, isReport, faults < maxFaults,
      todo' = List(), about' = about, creating' = false,
      faults' = faults + 1, dockerFailed' = dockerFailed,
      record' = record, lives' = lives, copy' = copy, behind' = behind,
      unchangedDocker, unchangedGhosts,
    }
  }

  action step = any {
    placed, deleted, abandoned, exits, falls, catchesUp, dies,
    plans, calls, dockerFails, reportIsLost,
  }

  // ---- What must hold after every step

  /// The agent never says CreateFailed of a GameServer that Docker did not
  /// fail. CreateFailed is final: said of a GameServer that may yet come
  /// up, it would throw that GameServer away.
  val noFailureIsMadeUp = not(complaint)

  /// The agent never takes down a running container of a GameServer that
  /// is doing fine, whatever it missed and however stale its copy.
  val nothingThatRunsIsTakenDown = not(tookDown)

  /// Containers are only ever started for the GameServer they were made for.
  val startedForTheirOwn =
    (todo.length() > 0 and Set(StartSidecar, StartGame).contains(todo.head())) implies
      (madeFor == about or (sidecar == NoSidecar and game == NoGame))

  val safe = and { noFailureIsMadeUp, nothingThatRunsIsTakenDown, startedForTheirOwn }

  // ---- What must come about in the end

  /// Left alone for long enough, the agent brings the containers of the
  /// GameServer up, and clears away those of none. "Left alone" is: its
  /// copy catches up, and it gets to plan and to make its calls.
  val settled = or {
    record.uid == 0 and sidecar == NoSidecar and game == NoGame and todo.length() == 0,
    record.uid != 0 and final(record.state) and record.stage == Live,
    record.uid != 0 and record.stage == Live and not(Set(Creating, Starting).contains(record.state)) and madeFor == record.uid,
  }

  temporal theAgentComesToRest = and {
    weakFair(plans, (record, lives, sidecar, game, madeFor, copy, behind, todo, about, creating, faults, dockerFailed, complaint, tookDown)),
    weakFair(calls, (record, lives, sidecar, game, madeFor, copy, behind, todo, about, creating, faults, dockerFailed, complaint, tookDown)),
    weakFair(catchesUp, (record, lives, sidecar, game, madeFor, copy, behind, todo, about, creating, faults, dockerFailed, complaint, tookDown)),
  } implies eventually(always(settled))

  // ---- Histories worth reading

  /// The ordinary start.
  run bringUpTest =
    init
      .then(placed)
      .then(plans)
      .then(6.reps(_ => calls))
      .expect(record.state == Scheduled and sidecar == SidecarRunning and game == GameRunning and todo.length() == 0)

  /// The agent dies between creating the containers and starting them. The
  /// next agent finds them, and takes the start up where it was left.
  run aStartThatWasCutShortTest =
    init
      .then(placed)
      .then(plans)
      .then(3.reps(_ => calls))
      .expect(record.state == Starting and game == GameCreated)
      .then(dies)
      .then(plans)
      .expect(todo == List(StartSidecar, StartGame, Report(ContainersRunning)))
      .then(3.reps(_ => calls))
      .expect(record.state == Scheduled and game == GameRunning)

  /// A GameServer is deleted and another is made under its name before the
  /// agent has looked. The containers of the first are cleared away, and
  /// not started for the second.
  run anEarlierOnesContainersTest =
    init
      .then(placed)
      .then(plans)
      .then(6.reps(_ => calls))
      .then(deleted)
      .then(abandoned)
      .then(placed)
      .expect(record.uid == 2 and madeFor == 1)
      .then(plans)
      .expect(todo == List(StopGame, RemoveAll))
      .then(2.reps(_ => calls))
      .then(plans)
      .expect(todo.head() == CreateSidecar)
}
