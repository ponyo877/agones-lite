// -*- mode: Bluespec; -*-

/// The store's file, and how far it is behind memory (pkg/store/save.go).
///
/// A write changes memory. Who puts it into the file depends on a mode the
/// Go code keeps in one flag: without Run, the write saves itself before it
/// returns, and a write the disk refuses is taken back. While Run is
/// running, writes return at once and Run saves what has gathered, in the
/// background. Whoever tells the outside world about a change calls Sync
/// first, which returns once the file has the change.
///
/// Sync does not take the store's lock while it waits. It reads two values
/// and waits on a channel, one after the other, while saves land in
/// between. The model keeps those steps apart: it is where a waiter could
/// miss the save it waits for, and sleep on.
module store {
  /// How many writes there are in one history, how often the disk refuses,
  /// and how often the process dies.
  pure val maxWrites = 3
  pure val maxRefusals = 2
  pure val maxCrashes = 1

  /// One caller of Sync, by how far it has come. Go: the loop of Sync.
  type Waiter =
    | Away
    | Began(int)             // it read the revision it waits for
    | Holding((int, int))    // and took the channel that the next move closes
    | Checked((int, int))    // and saw that the file is not there yet
    | Sleeping((int, int))   // and that Run is saving, so it waits on the channel
    | Saving(int)            // Run has ended: it saves by itself

  var memory: int        // the revision of the last change made: Go's Store.revision
  var file: int          // the revision the file holds: Go's Store.saved
  var behind: bool       // Run is saving: Go's Store.behind
  var round: int         // the revision Run's write in progress goes up to; 0 when it is not writing
  var moves: int         // how often the channel was closed and made anew: Go's tellWaiting
  var telling: bool      // a save has landed, and the channel is about to be closed
  var waiter: Waiter
  var ran: bool          // Run was started in this process
  // -- how much may still happen
  var writes: int
  var refusals: int
  var crashes: int
  // -- for the invariants
  var promised: int      // the highest revision a Sync has returned for: what the outside world was told

  action unchangedBounds = all { writes' = writes, refusals' = refusals, crashes' = crashes }
  action unchangedRun = all { behind' = behind, round' = round, ran' = ran }

  action init = all {
    memory' = 0, file' = 0, behind' = false, round' = 0, moves' = 0, telling' = false,
    waiter' = Away, ran' = false, writes' = 0, refusals' = 0, crashes' = 0, promised' = 0,
  }

  /// Go: Store.record without Run. The write saves itself, and everything
  /// else that is unsaved, before the lock is released.
  action writesThrough = all {
    not(behind), writes < maxWrites,
    memory' = memory + 1, file' = memory + 1, telling' = true,
    writes' = writes + 1, refusals' = refusals, crashes' = crashes,
    moves' = moves, waiter' = waiter, promised' = promised, unchangedRun,
  }

  /// The same, and the disk refuses. The change is taken back: to everyone
  /// else it never happened.
  action writeIsRefused = all {
    not(behind), writes < maxWrites, refusals < maxRefusals,
    writes' = writes + 1, refusals' = refusals + 1, crashes' = crashes,
    memory' = memory, file' = file, telling' = telling,
    moves' = moves, waiter' = waiter, promised' = promised, unchangedRun,
  }

  /// Go: Store.record with Run. The write changes memory and returns.
  action writesAhead = all {
    behind, writes < maxWrites,
    memory' = memory + 1, writes' = writes + 1, refusals' = refusals, crashes' = crashes,
    file' = file, telling' = telling, moves' = moves, waiter' = waiter, promised' = promised, unchangedRun,
  }

  /// Go: the beginning of Store.Run.
  action runBegins = all {
    not(ran),
    behind' = true, ran' = true, round' = 0,
    memory' = memory, file' = file, telling' = true, moves' = moves, waiter' = waiter,
    promised' = promised, unchangedBounds,
  }

  /// Go: the first half of saveBehind. Run takes what is unsaved and
  /// starts to write it, without the lock.
  action saveBegins = all {
    behind, round == 0, memory > file,
    round' = memory,
    behind' = behind, ran' = ran, memory' = memory, file' = file, telling' = telling,
    moves' = moves, waiter' = waiter, promised' = promised, unchangedBounds,
  }

  /// Go: the second half, settle. The write lands, and the file has it.
  action saveLands = all {
    round > 0,
    file' = round, round' = 0, telling' = true,
    behind' = behind, ran' = ran, memory' = memory, moves' = moves, waiter' = waiter,
    promised' = promised, unchangedBounds,
  }

  /// The disk refuses Run's write. It is tried again, whole.
  action saveIsRefused = all {
    round > 0, refusals < maxRefusals,
    round' = 0, refusals' = refusals + 1,
    writes' = writes, crashes' = crashes,
    behind' = behind, ran' = ran, memory' = memory, file' = file, telling' = telling,
    moves' = moves, waiter' = waiter, promised' = promised,
  }

  /// Go: tellWaiting. The channel is closed, and those who sleep look again.
  action tells = all {
    telling,
    telling' = false, moves' = moves + 1,
    memory' = memory, file' = file, waiter' = waiter, promised' = promised,
    unchangedRun, unchangedBounds,
  }

  /// Go: the end of Store.Run. What it has left is saved here, if the disk
  /// is willing, and those who sleep are told to look again either way.
  action runEnds(willing: bool): bool = all {
    behind, round == 0,
    willing or refusals < maxRefusals,
    behind' = false, round' = 0, ran' = ran,
    file' = if (willing) memory else file,
    refusals' = if (willing) refusals else refusals + 1,
    telling' = true,
    memory' = memory, moves' = moves, waiter' = waiter, promised' = promised,
    writes' = writes, crashes' = crashes,
  }

  /// The process dies, and another opens the file: memory is what the file
  /// had, and nobody is waiting any more.
  action crash = all {
    crashes < maxCrashes,
    crashes' = crashes + 1, writes' = writes, refusals' = refusals,
    memory' = file, behind' = false, round' = 0, ran' = false, telling' = false, waiter' = Away,
    file' = file, moves' = moves, promised' = promised,
  }

  /// Go: Store.Sync, one step of it.
  action syncs = match waiter {
    | Away => all { waiter' = Began(memory), promised' = promised, memory' = memory, file' = file, telling' = telling }
    | Began(upTo) => all { waiter' = Holding((upTo, moves)), promised' = promised, memory' = memory, file' = file, telling' = telling }
    | Holding(w) => all {
        waiter' = if (file >= w._1) Away else Checked(w),
        promised' = if (file >= w._1 and w._1 > promised) w._1 else promised,
        memory' = memory, file' = file, telling' = telling,
      }
    | Checked(w) => all {
        waiter' = if (behind) Sleeping(w) else Saving(w._1),
        promised' = promised, memory' = memory, file' = file, telling' = telling,
      }
    | Sleeping(w) => all {
        moves > w._2,
        waiter' = Began(w._1), promised' = promised, memory' = memory, file' = file, telling' = telling,
      }
    | Saving(upTo) => all {
        // Under the lock, so not while Run's write is on its way.
        round == 0,
        waiter' = Away, file' = memory, telling' = true,
        promised' = if (upTo > promised) upTo else promised,
        memory' = memory,
      }
  }

  action syncSteps = all { syncs, moves' = moves, unchangedRun, unchangedBounds }

  /// The caller of Sync stops waiting: whoever was to be told has gone.
  action givesUp = all {
    waiter != Away,
    waiter' = Away,
    memory' = memory, file' = file, telling' = telling, moves' = moves, promised' = promised,
    unchangedRun, unchangedBounds,
  }

  action step = any {
    writesThrough, writeIsRefused, writesAhead, runBegins, saveBegins, saveLands, saveIsRefused,
    tells, runEnds(true), runEnds(false), crash, syncSteps, givesUp,
  }

  // ---- What must hold after every step

  /// A crash can only take back what nobody outside was told: whatever a
  /// Sync has returned for is in the file.
  val whatWasPromisedIsSaved = promised <= file

  /// The file is never ahead of memory, and a write in progress writes
  /// nothing that was not made.
  val theFileFollows = file <= memory and round <= memory

  /// Nobody sleeps through the save they wait for: once the file has it,
  /// or Run has ended, the channel a sleeper holds is closed, or is about
  /// to be.
  val noSleeperIsLeftBehind = match waiter {
    | Sleeping(w) => (file >= w._1 or not(behind)) implies (moves > w._2 or telling)
    | _ => true
  }

  /// Without Run, nothing is ever left to somebody else to save: either the
  /// write saved itself or it never happened. Only a Run that ended on a
  /// disk that refused leaves something behind, for the next write or Sync.
  val writesAreWholeBeforeRun = (not(ran) and crashes == 0) implies memory == file

  val safe = and { whatWasPromisedIsSaved, theFileFollows, noSleeperIsLeftBehind, writesAreWholeBeforeRun }

  // ---- What must come about in the end

  /// A Sync returns: with the file caught up, or because its caller left.
  /// It does not wait for ever on a disk that works.
  temporal everySyncReturns = and {
    weakFair(saveBegins, (memory, file, behind, round, moves, telling, waiter, ran, writes, refusals, crashes, promised)),
    weakFair(saveLands, (memory, file, behind, round, moves, telling, waiter, ran, writes, refusals, crashes, promised)),
    weakFair(tells, (memory, file, behind, round, moves, telling, waiter, ran, writes, refusals, crashes, promised)),
    weakFair(syncSteps, (memory, file, behind, round, moves, telling, waiter, ran, writes, refusals, crashes, promised)),
  } implies always(eventually(waiter == Away))

  // ---- Histories worth reading

  /// A save lands between the two looks of a Sync. The waiter holds the
  /// channel from before, which the save closes: it wakes at once.
  run aSaveBetweenTwoLooksTest =
    init
      .then(runBegins).then(tells)
      .then(writesAhead)
      .then(syncSteps).then(syncSteps)
      .then(saveBegins)
      .then(syncSteps)
      .then(saveLands)
      .then(syncSteps)
      .expect(waiter == Sleeping((1, 1)) and telling)
      .then(tells)
      .then(syncSteps).then(syncSteps).then(syncSteps)
      .expect(waiter == Away and promised == 1)

  /// What was not saved is lost in a crash, and nobody had been told of it.
  run aCrashTakesBackOnlyWhatNobodyWasToldTest =
    init
      .then(runBegins).then(tells)
      .then(writesAhead)
      .then(saveBegins).then(saveLands).then(tells)
      .then(syncSteps).then(syncSteps).then(syncSteps)
      .expect(promised == 1)
      .then(writesAhead)
      .then(crash)
      .expect(memory == 1 and file == 1 and promised == 1)
}
