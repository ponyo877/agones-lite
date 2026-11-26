// -*- mode: Bluespec; -*-

/// What a game server asks through the SDK, on its way to the control plane.
///
/// The sidecar (pkg/sdkserver) answers the game server at once and queues
/// the request. It sends the queue in order, one request at a time, and
/// sends a request again until it hears that it arrived. So a request may
/// arrive twice: the first time it was carried out, and the answer got lost.
/// A Reserve with a time also starts a timer in the sidecar, which a later
/// request stops, if it is not too late for that.
///
/// The state these requests move belongs to the control plane, and a
/// matchmaker moves it too. The model checker tries every order of: the
/// game server calling the SDK, timers firing, requests arriving, answers
/// getting lost, the control plane's loop, and allocations.
module requests {
  import gameserverTables.* from "./gameserver"

  /// Whether the control plane tells a request it has carried out from a
  /// new one, by the name the sidecar gave it. The model is checked both
  /// ways: with `false`, it shows what the name is for.
  const NAMED: bool

  /// How many times the game server calls the SDK, and how many answers
  /// get lost. Enough for every race there is between two requests, a
  /// timer and an allocation.
  pure val maxCalls = 3
  pure val maxLost = 2

  /// One request in the sidecar's queue. id is 0 for what the sidecar says
  /// by itself; timer is the Reserve whose time ran out, for ReserveExpired.
  type Request = { event: Event, id: int, timer: int }

  // -- the sidecar
  var calls: int            // how many times the game server has called
  var reserve: int          // the call whose Reserve has a time: Go's Server.reserve; 0 for none
  var running: bool         // its timer has not gone off yet
  var fired: Set[int]       // timers that went off, and whose message is not yet queued or dropped
  var outbox: List[Request] // Go's Server.outbox
  // -- the control plane
  var state: State
  var lastRequest: int      // the last named request it carried out
  // -- for the invariants
  var arrived: Set[int]     // the requests that have arrived at least once
  var lost: int
  var queued: List[Request] // everything that was ever put in the queue, in order
  var was: State
  var cause: Request        // the request whose arrival was the last step, if it was one
  var repeat: bool          // whether that request had arrived before

  pure val none: Request = { event: Nothing, id: 0, timer: 0 }

  action unchangedSidecar = all {
    calls' = calls, reserve' = reserve, running' = running, fired' = fired, outbox' = outbox, queued' = queued,
  }

  /// A step that is not a request arriving.
  action elsewhere = all { was' = state, cause' = none, repeat' = false }

  action init = all {
    calls' = 0, reserve' = 0, running' = false, fired' = Set(), outbox' = List(),
    // The containers run, and the game server is about to call Ready.
    state' = Scheduled, lastRequest' = 0,
    arrived' = Set(), lost' = 0, queued' = List(),
    was' = Scheduled, cause' = none, repeat' = false,
  }

  /// Go: Server.ask. The game server calls Ready, Allocate, Shutdown or
  /// Reserve. Under one lock, the reservation before ends, a Reserve with a
  /// time begins one, and the request is queued.
  action ask(e: Event, timed: bool): bool = {
    val request = { event: e, id: calls + 1, timer: 0 }
    all {
      calls < maxCalls,
      calls' = calls + 1,
      reserve' = if (timed) calls + 1 else 0,
      running' = timed,
      // A timer that has gone off cannot be stopped: fired stays as it is.
      fired' = fired,
      outbox' = outbox.append(request),
      queued' = queued.append(request),
      state' = state, lastRequest' = lastRequest, arrived' = arrived, lost' = lost,
      elsewhere,
    }
  }

  action gameServerCalls = any {
    ask(SDKReady, false),
    ask(SDKAllocate, false),
    ask(SDKShutdown, false),
    ask(SDKReserve, false),
    ask(SDKReserve, true),
  }

  /// The time of the Reserve in force is up: its timer goes off, once.
  action timerFires = all {
    running,
    running' = false,
    fired' = fired.union(Set(reserve)),
    calls' = calls, reserve' = reserve, outbox' = outbox, queued' = queued,
    state' = state, lastRequest' = lastRequest, arrived' = arrived, lost' = lost,
    elsewhere,
  }

  /// Go: Server.expired. What a timer does once it has the lock: it says
  /// that the time is up, unless another request came first.
  action timerSpeaks = {
    nondet timer = oneOf(fired)
    val request = { event: ReserveExpired, id: 0, timer: timer }
    all {
      fired' = fired.exclude(Set(timer)),
      outbox' = if (reserve == timer) outbox.append(request) else outbox,
      queued' = if (reserve == timer) queued.append(request) else queued,
      calls' = calls, reserve' = reserve, running' = running,
      state' = state, lastRequest' = lastRequest, arrived' = arrived, lost' = lost,
      elsewhere,
    }
  }

  /// Go: Server.send, and Controller.Report at the other end. The request
  /// at the head of the queue arrives. When the answer comes back it leaves
  /// the queue; when the answer is lost it stays, and is sent again.
  action arrives(answered: bool): bool = {
    val request = outbox.head()
    val known = NAMED and request.id != 0 and request.id == lastRequest
    all {
      outbox.length() > 0,
      (answered or lost < maxLost),
      state' = if (known) state else nextState(state, request.event),
      lastRequest' = if (request.id != 0 and not(known)) request.id else lastRequest,
      outbox' = if (answered) outbox.tail() else outbox,
      lost' = if (answered) lost else lost + 1,
      arrived' = arrived.union(Set(request.id)),
      calls' = calls, reserve' = reserve, running' = running, fired' = fired, queued' = queued,
      was' = state, cause' = request, repeat' = (request.id != 0 and arrived.contains(request.id)),
    }
  }

  /// The control plane's own loop, and a matchmaker.
  action controlPlane(e: Event): bool = all {
    applies(state, e),
    state' = nextState(state, e),
    lastRequest' = lastRequest, arrived' = arrived, lost' = lost,
    unchangedSidecar, elsewhere,
  }

  action step = any {
    gameServerCalls, timerFires, timerSpeaks,
    arrives(true), arrives(false),
    controlPlane(Readied), controlPlane(AllocatedByRequest),
  }

  // ---- What must hold after every step

  /// A request the control plane has carried out does nothing when it
  /// arrives again. Above all it does not end a session: a game server that
  /// called Ready, was given to a match, and is back among the Ready ones
  /// because its Ready arrived twice, is given to a second match while the
  /// first one is being played.
  val aRepeatChangesNothing = repeat implies state == was

  /// The time of a Reserve running out never ends a session. A timer that
  /// has gone off cannot be stopped, so it may speak just as the game
  /// server allocates itself.
  val expiryEndsNoSession =
    (cause.event == ReserveExpired and was == Allocated) implies state == Allocated

  /// "The time is up" is queued before any request made after its Reserve,
  /// or not at all. Queued after a new Reserve, it would end the
  /// reservation the game server has just made.
  val expiryComesFirst = queued.indices().forall(i => queued.indices().forall(j =>
    (i < j and queued[j].event == ReserveExpired) implies queued[i].id <= queued[j].timer
  ))

  /// The queue keeps the order the game server called in.
  val inTheOrderAsked = queued.indices().forall(i => queued.indices().forall(j =>
    (i < j and queued[i].id != 0 and queued[j].id != 0) implies queued[i].id < queued[j].id
  ))

  val safe = and { aRepeatChangesNothing, expiryEndsNoSession, expiryComesFirst, inTheOrderAsked }

  // ---- Races worth reading

  /// The game server allocates itself just as its reservation runs out.
  /// The timer has gone off and cannot be stopped; it finds another request
  /// in its place and says nothing.
  run allocateAsTheReservationRunsOutTest =
    init
      .then(ask(SDKReserve, true))
      .then(timerFires)
      .then(ask(SDKAllocate, false))
      .then(timerSpeaks)
      .expect(outbox.length() == 2 and outbox[1].event == SDKAllocate)
      .then(arrives(true))
      .then(arrives(true))
      .expect(state == Allocated)

  /// A Ready whose answer was lost arrives a second time, after a
  /// matchmaker was given the game server.
  run readyArrivesTwiceTest =
    init
      .then(ask(SDKReady, false))
      .then(arrives(false))
      .then(controlPlane(Readied))
      .then(controlPlane(AllocatedByRequest))
      .expect(state == Allocated)
      .then(arrives(true))
      .expect(state == (if (NAMED) Allocated else RequestReady))
}

/// The requests as the control plane takes them: each one once.
module requestsNamed {
  import requests(NAMED = true).*
}

/// The requests as they would be taken without their names: a request that
/// arrives twice is carried out twice. The model checker finds the session
/// that ends for it.
module requestsUnnamed {
  import requests(NAMED = false).*
}
