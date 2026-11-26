// -*- mode: Bluespec; -*-

/// The GameServer as a game server sees it through the SDK
/// (pkg/sdkserver/view.go).
///
/// A change to a Counter shows in the next read, long before the control
/// plane has made it: the sidecar lays the changes that are on their way
/// over the last GameServer it was given. That GameServer comes from two
/// sides. The agent pushes one after every change, and the answer to a
/// change is the GameServer with that change made. A pushed one may or may
/// not have the change that is being sent, and only the answer tells, so
/// while changes are on their way a pushed GameServer is held back.
///
/// The Go code keeps this in four fields: current, pending, held and view.
/// The model follows one Counter that every change adds one to, and looks
/// for an order of pushes, answers and lost answers in which the game
/// server is shown a count that has a change twice, or not at all.
module view {
  pure val maxChanges = 2     // changes the game server makes
  pure val maxOthers = 2      // changes somebody else makes to the GameServer
  pure val maxLost = 1        // answers that get lost

  /// A GameServer at one version: its resourceVersion and its Counter.
  type GameServer = { version: int, count: int }

  pure val none: GameServer = { version: 0, count: 0 }

  // -- the control plane
  var stored: GameServer
  var made: int -> int          // the changes it has made, by name, and the version each was made in
  // -- on the way to the sidecar
  var pushes: List[GameServer]  // what the agent has pushed and the sidecar has not read, in order
  var answer: GameServer        // the answer to the change being sent, when it is on its way; none otherwise
  // -- the sidecar
  var current: GameServer       // Go: Server.current
  var pending: List[int]        // Go: Server.pending. The first is the one being sent
  var held: GameServer          // Go: Server.held
  // -- how much may still happen
  var changes: int
  var others: int
  var lost: int

  /// Go: older. Without two versions to compare, a is not older.
  pure def older(a: GameServer, b: GameServer): bool = a.version != 0 and b.version != 0 and a.version < b.version

  /// Go: Server.adopt. A GameServer becomes the base, unless the base is
  /// newer.
  pure def adopt(base: GameServer, gs: GameServer): GameServer =
    if (gs.version == 0 or older(gs, base)) base else gs

  /// Go: WithPending and Server.show. What the game server is shown.
  val shown = current.count + pending.length()

  action unchangedBounds = all { changes' = changes, others' = others, lost' = lost }

  action init = all {
    stored' = { version: 1, count: 0 }, made' = Map(),
    pushes' = List({ version: 1, count: 0 }), answer' = none,
    current' = none, pending' = List(), held' = none,
    changes' = 0, others' = 0, lost' = 0,
  }

  /// Go: Server.change. The game server adds one to the Counter. The
  /// change shows at once and is queued for the agent.
  action changed = all {
    current.version != 0, changes < maxChanges,
    pending' = pending.append(changes + 1),
    changes' = changes + 1, others' = others, lost' = lost,
    stored' = stored, made' = made, pushes' = pushes, answer' = answer,
    current' = current, held' = held,
  }

  /// Go: Controller.UpdateCounts. The change being sent arrives, is made
  /// unless it was made before, and is answered with the GameServer as it
  /// is then. The agent pushes the new version as well.
  action arrives = {
    val change = pending.head()
    val again = made.keys().contains(change)
    val after = if (again) stored else { version: stored.version + 1, count: stored.count + 1 }
    all {
      pending.length() > 0, answer.version == 0,
      stored' = after, made' = if (again) made else made.put(change, after.version),
      answer' = after,
      pushes' = if (again) pushes else pushes.append(after),
      current' = current, pending' = pending, held' = held, unchangedBounds,
    }
  }

  /// The answer is lost. The sidecar sends the change again.
  action answerIsLost = all {
    answer.version != 0, lost < maxLost,
    answer' = none, lost' = lost + 1, changes' = changes, others' = others,
    stored' = stored, made' = made, pushes' = pushes,
    current' = current, pending' = pending, held' = held,
  }

  /// Go: Server.settle. The answer arrives: the change is no longer on its
  /// way, the answer becomes the base, and so does what was held back.
  action answered = all {
      answer.version != 0,
      current' = adopt(adopt(current, answer), held), pending' = pending.tail(), held' = none, answer' = none,
      stored' = stored, made' = made, pushes' = pushes, unchangedBounds,
  }

  /// Somebody else changes the GameServer: its state, or the Counter by an
  /// allocation. The agent pushes that too.
  action changedElsewhere = {
    nondet add = oneOf(Set(0, 1))
    val after = { version: stored.version + 1, count: stored.count + add }
    all {
      others < maxOthers,
      stored' = after, pushes' = pushes.append(after),
      others' = others + 1, changes' = changes, lost' = lost,
      made' = made, answer' = answer, current' = current, pending' = pending, held' = held,
    }
  }

  /// The agent replaces a push the sidecar has not read by the next one:
  /// only the latest matters.
  action pushIsSkipped = all {
    pushes.length() > 1,
    pushes' = pushes.tail(),
    stored' = stored, made' = made, answer' = answer,
    current' = current, pending' = pending, held' = held, unchangedBounds,
  }

  /// Go: Server.set. A pushed GameServer arrives. While changes are on
  /// their way it is held back; otherwise it becomes the base.
  action pushed = {
    val gs = pushes.head()
    val waits = pending.length() > 0
    all {
      pushes.length() > 0,
      pushes' = pushes.tail(),
      held' = if (waits and not(older(gs, held))) gs else held,
      current' = if (waits) current else adopt(current, gs),
      stored' = stored, made' = made, answer' = answer, pending' = pending, unchangedBounds,
    }
  }

  action step = any { changed, arrives, answerIsLost, answered, changedElsewhere, pushIsSkipped, pushed }

  // ---- What must hold after every step

  /// Whether the base has a change in it: the control plane made the change
  /// in a version no later than the base.
  def inTheBase(change: int): bool = made.keys().contains(change) and made.get(change) <= current.version

  def onItsWay(change: int): bool = pending.indices().exists(i => pending[i] == change)

  /// No change is counted twice: the base never has a change in it that is
  /// still laid on top. This is what a pushed GameServer is held back for.
  val noChangeTwice = pending.indices().forall(i => not(inTheBase(pending[i])))

  /// And none is lost: a change that is no longer on its way is in the
  /// base, so the count does not drop back when its answer arrives.
  val noChangeLost = made.keys().forall(change => onItsWay(change) or inTheBase(change))

  /// A GameServer is only held back while a change is on its way.
  val heldForAReason = held.version != 0 implies pending.length() > 0

  val safe = and { noChangeTwice, noChangeLost, heldForAReason }

  // ---- Histories worth reading

  /// The agent pushes a GameServer that already has the change being sent,
  /// before the answer to that change arrives. Laid under the change, it
  /// would count the change twice; so it waits.
  run aPushThatHasTheChangeTest =
    init
      .then(pushed)
      .then(changed)
      .expect(shown == 1)
      .then(arrives)
      .then(pushed)
      .expect(current.count == 0 and held.count == 1 and shown == 1)
      .then(answered)
      .expect(current.count == 1 and pending.length() == 0 and shown == 1)

  /// An answer is lost and the change is sent again. The control plane
  /// knows it by its name, and makes it once.
  run aChangeThatIsSentAgainTest =
    init
      .then(pushed)
      .then(changed)
      .then(arrives)
      .then(answerIsLost)
      .then(arrives)
      .then(answered)
      .expect(stored.count == 1 and shown == 1)
}
