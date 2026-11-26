// -*- mode: Bluespec; -*-

/// How a Fleet goes away, and takes what it owns with it.
///
/// Every resource is deleted in two steps: its deletion is asked for, and
/// its controller drops the record once nothing is left that belongs to it.
/// Kubernetes does this with owner references and a garbage collector. Here
/// each controller does it for its own kind, in its dissolve function:
/// pkg/fleets/controller.go for a Fleet and its GameServerSets,
/// pkg/gameserversets/controller.go for a set and its GameServers, and
/// pkg/gameservers/lifecycle.go for a GameServer and its containers.
///
/// Meanwhile the same controllers keep making what is missing: a Fleet its
/// set, a set its GameServers. The model lets all of them run in any order
/// while an operator deletes the Fleet, and looks for something left behind.
module cascade {
  import gameserverTables.* from "./gameserver"

  /// How many sets and GameServers are made in one history.
  pure val maxSets = 2
  pure val maxServers = 3

  type Server = { owner: int, stage: Stage, session: bool }

  var fleet: Stage
  var sets: List[Stage]         // every set the Fleet has made, by the order it made them in
  var servers: List[Server]     // every GameServer the sets have made

  /// The GameServers set number i owns that are not gone.
  def owned(i: int): Set[int] = servers.indices().filter(s => servers[s].owner == i and servers[s].stage != Gone)

  /// The same of the Fleet and its sets.
  def held = sets.indices().filter(i => sets[i] != Gone)

  action init = all { fleet' = Live, sets' = List(), servers' = List() }

  /// An operator deletes the Fleet.
  action fleetIsDeleted = all { fleet == Live, fleet' = Leaving, sets' = sets, servers' = servers }

  /// Go: fleets.Controller.sync. A Fleet that is leaving asks for the
  /// deletion of its sets, and is dropped once it has none. One that stays
  /// makes a set when none of its sets is staying.
  action fleetSyncs = any {
    all {
      fleet == Leaving, held.size() > 0,
      sets' = sets.foldl(List(), (out, stage) => out.append(if (stage == Live) Leaving else stage)),
      fleet' = fleet, servers' = servers,
    },
    all { fleet == Leaving, held.size() == 0, fleet' = Gone, sets' = sets, servers' = servers },
    all {
      fleet == Live, sets.length() < maxSets, sets.indices().forall(i => sets[i] != Live),
      sets' = sets.append(Live), fleet' = fleet, servers' = servers,
    },
  }

  /// Go: gameserversets.Controller.sync, for one set. A set that is leaving
  /// asks for the deletion of every GameServer it owns, sessions and all,
  /// and is dropped once it owns none. One that stays makes a GameServer
  /// when it has none that stays.
  action setSyncs = {
    nondet i = oneOf(sets.indices())
    any {
      all {
        sets[i] == Leaving, owned(i).size() > 0,
        servers' = servers.foldl(List(), (out, gs) =>
          out.append(if (gs.owner == i and gs.stage == Live) { ...gs, stage: Leaving } else gs)),
        sets' = sets, fleet' = fleet,
      },
      all { sets[i] == Leaving, owned(i).size() == 0, sets' = sets.replaceAt(i, Gone), servers' = servers, fleet' = fleet },
      all {
        sets[i] == Live, servers.length() < maxServers, owned(i).forall(s => servers[s].stage != Live),
        servers' = servers.append({ owner: i, stage: Live, session: false }),
        sets' = sets, fleet' = fleet,
      },
    }
  }

  /// A matchmaker is given a GameServer, or its session ends by a
  /// Shutdown, which asks for its deletion.
  action serverChanges = {
    nondet s = oneOf(servers.indices())
    nondet ends = oneOf(Set(true, false))
    all {
      servers[s].stage == Live,
      servers' = servers.replaceAt(s, if (ends) { ...servers[s], stage: Leaving } else { ...servers[s], session: true }),
      sets' = sets, fleet' = fleet,
    }
  }

  /// Go: gameservers. The containers of a GameServer that is leaving are
  /// removed, or the control plane stops waiting for that, and the record
  /// is dropped.
  action serverGoes = {
    nondet s = oneOf(servers.indices())
    all {
      servers[s].stage == Leaving,
      servers' = servers.replaceAt(s, { ...servers[s], stage: Gone }),
      sets' = sets, fleet' = fleet,
    }
  }

  action step = any { fleetIsDeleted, fleetSyncs, setSyncs, serverChanges, serverGoes }

  // ---- What must hold after every step

  /// Nothing outlives its owner: a set is dropped only when it owns
  /// nothing, and a Fleet only when it has no set.
  val nothingIsOrphaned = and {
    servers.indices().forall(s => sets[servers[s].owner] == Gone implies servers[s].stage == Gone),
    fleet == Gone implies sets.indices().forall(i => sets[i] == Gone),
  }

  /// A Fleet has one set that stays at most: two would both keep the
  /// Fleet's size, and it would have twice the GameServers it asks for.
  val oneSetStays = sets.indices().filter(i => sets[i] == Live).size() <= 1

  /// A Fleet that is gone has taken every GameServer with it.
  val aFleetTakesEverythingWithIt = fleet == Gone implies servers.indices().forall(s => servers[s].stage == Gone)

  val safe = and { nothingIsOrphaned, oneSetStays, aFleetTakesEverythingWithIt }

  // ---- What must come about in the end

  /// A Fleet whose deletion was asked for goes, and so does everything it
  /// owned, however many sessions were on.
  temporal aDeletedFleetGoes = and {
    weakFair(fleetSyncs, (fleet, sets, servers)),
    weakFair(setSyncs, (fleet, sets, servers)),
    weakFair(serverGoes, (fleet, sets, servers)),
  } implies always(fleet == Leaving implies eventually(fleet == Gone))

  // ---- Histories worth reading

  run aFleetWithASessionIsDeletedTest =
    init
      .then(fleetSyncs)
      .then(setSyncs)
      .expect(servers.length() == 1 and servers[0].stage == Live)
      .then(fleetIsDeleted)
      .then(fleetSyncs)
      .expect(sets[0] == Leaving)
      .then(setSyncs)
      .expect(servers[0].stage == Leaving)
      .then(serverGoes)
      .then(setSyncs)
      .expect(sets[0] == Gone)
      .then(fleetSyncs)
      .expect(fleet == Gone)
}
