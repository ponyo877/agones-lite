// -*- mode: Bluespec; -*-

/// The work queue every controller runs on (pkg/queue).
///
/// A key is in one of a few states that the Go code keeps in three places:
/// a set of the keys that wait, their order, and a count of failures. Here
/// they are spelled out. A key waits, is handled, and after a failure comes
/// back by a timer. Whoever adds a key wakes the worker through a channel
/// that holds one token; the worker sleeps on that channel when it finds
/// nothing waiting.
///
/// Adding takes two steps in Go, the lock and then the token, and the
/// worker may run between them. The model keeps the two apart, which is
/// where a wake-up could get lost.
module queue {
  type Key = A | B

  pure val allKeys = Set(A, B)

  /// How often keys are added from outside, and how often handling fails.
  pure val maxAdds = 3
  pure val maxFailures = 2

  /// What the worker is doing. Go: the loop of Queue.Run and Queue.next.
  type Worker =
    | Looking         // about to pop
    | Asleep          // found nothing, and waits for a token
    | Handling(Key)

  var waiting: Set[Key]   // Go: Queue.waiting
  var order: List[Key]    // Go: Queue.order
  var token: bool         // Go: Queue.wake holds its one token
  var worker: Worker
  var timers: Set[Key]    // Go: the timers of AddAfter that have not fired
  var waking: int         // callers of Add between the lock and the token
  // -- how much may still happen
  var adds: int
  var failures: int
  // -- for the invariants
  var owed: Set[Key]      // added since the worker last began to handle them
  var handled: Set[Key]

  action unchangedBounds = all { adds' = adds, failures' = failures }

  action init = all {
    waiting' = Set(), order' = List(), token' = false, worker' = Looking, timers' = Set(), waking' = 0,
    adds' = 0, failures' = 0, owed' = Set(), handled' = Set(),
  }

  /// Go: the first half of Queue.Add, under the lock. A key that waits
  /// already is not added twice.
  action enqueue(key: Key): bool = all {
    waiting' = waiting.union(Set(key)),
    order' = if (waiting.contains(key)) order else order.append(key),
    waking' = waking + 1,
    owed' = owed.union(Set(key)),
    token' = token, worker' = worker, handled' = handled,
  }

  /// Something changed, and a controller adds the key.
  action added = all {
    adds < maxAdds,
    nondet key = oneOf(allKeys)
    enqueue(key),
    adds' = adds + 1, failures' = failures, timers' = timers,
  }

  /// Go: the second half of Queue.Add. The token is put in, unless one is
  /// there already.
  action wakes = all {
    waking > 0,
    waking' = waking - 1, token' = true,
    waiting' = waiting, order' = order, worker' = worker, timers' = timers,
    owed' = owed, handled' = handled, unchangedBounds,
  }

  /// Go: Queue.pop. The worker takes the key that has waited longest, or
  /// finds none and goes to sleep.
  action pops = all {
    worker == Looking,
    worker' = if (order.length() == 0) Asleep else Handling(order.head()),
    order' = if (order.length() == 0) order else order.tail(),
    waiting' = if (order.length() == 0) waiting else waiting.exclude(Set(order.head())),
    owed' = if (order.length() == 0) owed else owed.exclude(Set(order.head())),
    token' = token, timers' = timers, waking' = waking, handled' = handled, unchangedBounds,
  }

  /// Go: the select in Queue.next. A token wakes the worker.
  action wakesUp = all {
    worker == Asleep, token,
    worker' = Looking, token' = false,
    waiting' = waiting, order' = order, timers' = timers, waking' = waking,
    owed' = owed, handled' = handled, unchangedBounds,
  }

  /// Go: Queue.finish. The handler returns. After a failure the key comes
  /// back by a timer.
  action finishes(failed: bool): bool = match worker {
    | Handling(key) => all {
        failed implies failures < maxFailures,
        worker' = Looking,
        timers' = if (failed) timers.union(Set(key)) else timers,
        failures' = if (failed) failures + 1 else failures,
        handled' = if (failed) handled else handled.union(Set(key)),
        waiting' = waiting, order' = order, token' = token, waking' = waking,
        owed' = owed, adds' = adds,
      }
    | _ => all {
        false,
        worker' = worker, timers' = timers, failures' = failures, handled' = handled,
        waiting' = waiting, order' = order, token' = token, waking' = waking, owed' = owed, adds' = adds,
      }
  }

  /// Go: Queue.AddAfter. The timer of a failed key fires.
  action retries = all {
    nondet key = oneOf(timers)
    all { enqueue(key), timers' = timers.exclude(Set(key)) },
    unchangedBounds,
  }

  action step = any { added, wakes, pops, wakesUp, finishes(false), finishes(true), retries }

  // ---- What must hold after every step

  /// The set and the order say the same, and no key waits twice: a burst
  /// of changes to one resource costs a single pass.
  val eachKeyWaitsOnce = and {
    order.indices().forall(i => order.indices().forall(j => i != j implies order[i] != order[j])),
    order.indices().map(i => order[i]) == waiting,
  }

  /// No wake-up is lost: the worker does not sleep on a key that waits
  /// unless a token is there for it, or on its way.
  val noWakeUpIsLost = (worker == Asleep and order.length() > 0) implies (token or waking > 0)

  /// Whatever is owed a look is somewhere it will be found.
  val nothingOwedIsForgotten = owed.subseteq(waiting)

  val safe = and { eachKeyWaitsOnce, noWakeUpIsLost, nothingOwedIsForgotten }

  // ---- What must come about in the end

  /// Every key that is added is handled, and one whose handler fails is
  /// handled again.
  temporal everyKeyGetsItsTurn = and {
    weakFair(wakes, (waiting, order, token, worker, timers, waking, adds, failures, owed, handled)),
    weakFair(pops, (waiting, order, token, worker, timers, waking, adds, failures, owed, handled)),
    weakFair(wakesUp, (waiting, order, token, worker, timers, waking, adds, failures, owed, handled)),
    weakFair(finishes(false), (waiting, order, token, worker, timers, waking, adds, failures, owed, handled)),
    weakFair(retries, (waiting, order, token, worker, timers, waking, adds, failures, owed, handled)),
  } implies always(eventually(owed == Set() and timers == Set()))

  // ---- Histories worth reading

  /// A key is added just as the worker finds nothing and goes to sleep.
  /// The token arrives after it has dozed off, and wakes it.
  run addedAsTheWorkerDozesOffTest =
    init
      .then(all { enqueue(A), adds' = adds + 1, failures' = failures, timers' = timers })
      .then(pops)
      .then(finishes(false))
      .then(all { enqueue(B), adds' = adds + 1, failures' = failures, timers' = timers })
      .then(wakes)
      .then(wakes)
      .then(pops)
      .then(finishes(false))
      .then(pops)
      .expect(worker == Asleep and token)
      .then(wakesUp)
      .then(pops)
      .expect(worker == Asleep and not(token) and handled == Set(A, B))
}
