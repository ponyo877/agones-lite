// -*- mode: Bluespec; -*-

/// How the sidecar notices a game server that stopped sending Health pings
/// (pkg/sdkserver/health.go).
///
/// In Go the stages are the lines of one function, watchHealth: it waits for
/// the first GameServer, returns if the check is disabled, sleeps through
/// the initial delay, and then checks once a period until it has reported.
/// Here they are spelled out. Time is counted in periods.
module health {
  /// spec.health.failureThreshold.
  pure val threshold = 2
  pure val maxTime = 5

  type Stage =
    | Arriving     // the sidecar has no GameServer yet, so no rules
    | Disabled     // spec.health.disabled: nothing is ever checked
    | Delaying     // spec.health.initialDelaySeconds have not passed
    | Checking     // a check every period
    | Reported     // HealthFailed was said, once, and the checks have ended

  var stage: Stage
  var failures: int       // Go: health.failures
  var lastPing: int       // Go: health.lastPing
  var now: int
  // -- for the invariants
  var silent: int         // how many periods in a row have ended without a ping
  var pinged: bool        // a ping came in the period that is running
  var said: int           // how often HealthFailed was said
  var atReport: int       // how long the silence had lasted when it was said

  action init = all {
    stage' = Arriving, failures' = 0, lastPing' = 0, now' = 0,
    silent' = 0, pinged' = false, said' = 0, atReport' = 0,
  }

  /// The first GameServer arrives from the agent, with its rules.
  action arrives(disabled: bool): bool = all {
    stage == Arriving,
    stage' = if (disabled) Disabled else Delaying,
    failures' = failures, lastPing' = lastPing, now' = now, silent' = silent, pinged' = pinged, said' = said,
    atReport' = atReport,
  }

  /// The initial delay is over. Go: the touch before the ticker starts.
  action delayEnds = all {
    stage == Delaying,
    stage' = Checking, lastPing' = now, failures' = 0,
    now' = now, silent' = 0, pinged' = true, said' = said, atReport' = atReport,
  }

  /// Go: health.touch. The game server pings, which it may at any stage.
  action pings = all {
    lastPing' = now, failures' = 0,
    stage' = stage, now' = now, silent' = 0, pinged' = true, said' = said, atReport' = atReport,
  }

  /// Go: health.check and Server.failed. A period ends. While checking, it
  /// is a failure when no ping came within it, and at the threshold the
  /// sidecar says so and stops.
  action periodEnds = {
    val missed = lastPing + 1 < now + 1
    val count = if (stage == Checking and missed) failures + 1 else failures
    val over = stage == Checking and count >= threshold
    all {
      now < maxTime,
      now' = now + 1,
      failures' = count,
      stage' = if (over) Reported else stage,
      said' = if (over) said + 1 else said,
      atReport' = if (over) (if (pinged) 0 else silent + 1) else atReport,
      silent' = if (pinged) 0 else silent + 1,
      pinged' = false,
      lastPing' = lastPing,
    }
  }

  action step = any { arrives(true), arrives(false), delayEnds, pings, periodEnds }

  // ---- What must hold after every step

  /// A game server is reported only after as many periods in a row without
  /// a ping as the threshold says, and any ping starts the count again.
  val reportedOnlyAfterSilence = said > 0 implies atReport >= threshold

  /// The count is of periods in a row: never more than have gone by in
  /// silence.
  val failuresAreInARow = stage == Checking implies failures <= silent

  /// HealthFailed is said once at most. Unhealthy is final, and the checks
  /// end with it.
  val saidOnce = said <= 1 and (said == 1 iff stage == Reported)

  /// Nothing is checked before the rules are known, during the initial
  /// delay, or when the check is switched off.
  val notBeforeItsTime = Set(Arriving, Disabled, Delaying).contains(stage) implies (failures == 0 and said == 0)

  val safe = and { reportedOnlyAfterSilence, failuresAreInARow, saidOnce, notBeforeItsTime }

  // ---- Histories worth reading

  /// Two periods without a ping, and the game server is reported.
  run twoSilentPeriodsTest =
    init
      .then(arrives(false))
      .then(delayEnds)
      .then(periodEnds).then(periodEnds)
      .expect(stage == Checking and failures == 1)
      .then(periodEnds)
      .expect(stage == Reported and said == 1)

  /// A ping in every other period keeps a game server alive at a threshold
  /// of two.
  run aPingInTimeTest =
    init
      .then(arrives(false))
      .then(delayEnds)
      .then(periodEnds).then(periodEnds)
      .expect(failures == 1 and stage == Checking)
      .then(pings)
      .then(periodEnds).then(periodEnds)
      .expect(failures == 1 and stage == Checking)

  /// With the check switched off, silence is nothing to report.
  run switchedOffTest =
    init
      .then(arrives(true))
      .then(3.reps(_ => periodEnds))
      .expect(stage == Disabled and said == 0)
}
