// -*- mode: Bluespec; -*-

/// The copy an agent on another machine keeps of what is placed on its node.
///
/// The agent holds one Watch call open (pkg/nodes/remote.go). The control
/// plane answers it with everything on the node, then "synced", then every
/// change (pkg/nodes/server.go). Until "synced" the agent collects aside
/// and touches nothing; afterwards it acts on its copy, and removes the
/// containers of whatever the copy does not have.
///
/// The model follows one name on one node. A GameServer of that name is
/// made, changed, deleted and made again, the call breaks and is made
/// again, and messages arrive whenever they like.
module link {
  /// Whether the list the control plane sends ends exactly where the
  /// changes it sends begin. If the list is read later than the changes
  /// were asked for, the changes in between are in the list and are sent
  /// once more behind it.
  const TOGETHER: bool

  pure val maxChanges = 5
  pure val maxCalls = 2

  /// What a name holds at one moment: which GameServer, by a number that
  /// stands for its uid, and the revision of the store that wrote it.
  /// Nothing is uid 0.
  type Entry = { uid: int, version: int }

  pure val nothing: Entry = { uid: 0, version: 0 }

  /// One change, as a watcher is told of it. A removal carries what the
  /// name last held.
  type Change = Updated(Entry) | Removed(Entry)

  /// The Watch call. Go has no word for the three: Remote.current tells the
  /// last from the other two, and a call that is open is a goroutine.
  type Call =
    | Down                // none is open: the copy is stale
    | Asked(int)          // the changes from this place in the log on were asked for; the list is not read yet
    | Live(int)           // the list and "synced" were sent; next comes the change at this place

  // -- the control plane
  var held: Entry           // what the store has under the name
  var log: List[Change]     // every change to it, in order
  var uids: int             // how many GameServers the name has had
  // -- the link
  var call: Call
  var calls: int
  // -- the agent
  var copy: Entry
  var current: bool         // Go: Remote.current
  // -- for the invariants
  var listed: int           // the revision of the store when the list of this call was read

  action unchangedStore = all { held' = held, log' = log, uids' = uids }
  action unchangedAgent = all { copy' = copy, current' = current, listed' = listed }

  action init = all {
    held' = nothing, log' = List(), uids' = 0,
    call' = Down, calls' = 0,
    copy' = nothing, current' = false, listed' = 0,
  }

  /// A GameServer of the name is made, or the one there is changed.
  action written = {
    val revision = log.length() + 1
    val entry = { uid: if (held.uid == 0) uids + 1 else held.uid, version: revision }
    all {
      log.length() < maxChanges,
      held' = entry,
      log' = log.append(Updated(entry)),
      uids' = if (held.uid == 0) uids + 1 else uids,
      call' = call, calls' = calls, unchangedAgent,
    }
  }

  /// The one there is dropped from the store.
  action dropped = all {
    log.length() < maxChanges, held.uid != 0,
    held' = nothing,
    log' = log.append(Removed(held)),
    uids' = uids,
    call' = call, calls' = calls, unchangedAgent,
  }

  /// Go: Remote.watch and the first lines of Server.Watch. The agent calls,
  /// and the control plane begins to keep the changes for it.
  action asks = all {
    call == Down, calls < maxCalls,
    call' = Asked(log.length()),
    calls' = calls + 1,
    unchangedStore, unchangedAgent,
  }

  /// Go: Server.sendPlaced, and Remote.take for "synced". The control plane
  /// reads what is on the node and sends it; the agent takes it for its
  /// copy. Read together with the asking, nothing has changed in between.
  action lists = match call {
    | Asked(from) => all {
        TOGETHER implies from == log.length(),
        copy' = held, current' = true, listed' = log.length(),
        call' = Live(from),
        calls' = calls, unchangedStore,
      }
    | _ => all { false, call' = call, calls' = calls, unchangedStore, unchangedAgent }
  }

  /// Go: forward, and Remote.take for a change. The next change arrives.
  action forwards = match call {
    | Live(at) => all {
        at < log.length(),
        copy' = match log[at] { | Updated(entry) => entry | Removed(_) => nothing },
        call' = Live(at + 1),
        current' = current, listed' = listed,
        calls' = calls, unchangedStore,
      }
    | _ => all { false, call' = call, calls' = calls, unchangedStore, unchangedAgent }
  }

  /// Go: the end of Remote.watch. The call breaks. The copy stays as it
  /// is, and is not acted on until another call has brought a whole list.
  action breaks = all {
    call != Down,
    call' = Down, current' = false,
    copy' = copy, listed' = listed,
    calls' = calls, unchangedStore,
  }

  action step = any { written, dropped, asks, lists, forwards, breaks }

  // ---- What must hold after every step

  /// How far behind the copy is: the place in the log it is as of.
  val seen = match call {
    | Live(at) => at
    | _ => 0
  }

  /// What the name held at a place in the log.
  def heldAt(at: int): Entry =
    if (at == 0) nothing
    else match log[at - 1] { | Updated(entry) => entry | Removed(_) => nothing }

  /// A copy that is acted on is what the store held at some moment since
  /// its list was read: late perhaps, never wrong. An agent that saw the
  /// name hold nothing when it holds a GameServer would remove that
  /// GameServer's containers; one that saw an earlier GameServer of the
  /// name marked for deletion would do the same.
  val theCopyIsOnlyEverLate = current implies and {
    seen >= listed implies copy == heldAt(seen),
    seen < listed implies copy == heldAt(listed),
  }

  /// A copy that is acted on never shows the name holding nothing while it
  /// has held the same GameServer ever since the list.
  val nothingDisappearsFromTheCopy =
    (current and held.uid != 0 and heldAt(listed).uid == held.uid) implies copy.uid == held.uid

  val safe = and { theCopyIsOnlyEverLate, nothingDisappearsFromTheCopy }

  // ---- What must come about in the end

  /// While the call lasts, the copy catches up with the store.
  temporal theCopyCatchesUp =
    weakFair(forwards, (held, log, uids, call, calls, copy, current, listed))
      implies always(eventually(call == Down or copy == held or not(current)))

  // ---- Histories worth reading

  /// A GameServer is deleted and made again under its name while the agent
  /// is calling. The list has the new one. If the changes are sent from
  /// before the list, the removal of the old one arrives behind it and
  /// takes the new one out of the copy.
  run aRemovalFromBeforeTheListTest =
    init
      .then(written)
      .then(asks)
      .then(dropped)
      .then(written)
      .then(
        if (TOGETHER) lists.fail()
        else lists.then(forwards).expect(copy == nothing and held.uid == 2)
      )

  run theOrdinaryCallTest =
    init
      .then(written)
      .then(asks)
      .then(lists)
      .expect(current and copy == held)
      .then(written)
      .then(forwards)
      .expect(copy == held)
      .then(dropped)
      .then(forwards)
      .expect(copy == nothing)
}

/// The link as the control plane serves it: the list and the changes meet.
module linkTogether {
  import link(TOGETHER = true).*
}

/// The link with the list read after the changes were asked for. The model
/// checker finds the GameServer that vanishes from the copy.
module linkApart {
  import link(TOGETHER = false).*
}
