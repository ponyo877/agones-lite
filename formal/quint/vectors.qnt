// -*- mode: Bluespec; -*-

/// What the models answer, written down for the Go tests.
///
/// A check of a model says nothing about the Go code unless the two behave
/// alike. The tables of the models are small enough to write out whole:
/// every input there is, with the answer. `build/ci/models.sh vectors`
/// runs this module and turns each variable into a file of
/// formal/vectors/quint, one JSON object to the line, and Go tests replay
/// those files against the real functions.
module vectors {
  import gameserverTables from "./gameserver"
  import agentTables from "./agent"
  import nodeTables from "./node"

  // pkg/gameservers: Next, NextStage and decide.
  var transitions: Set[{ state: gameserverTables::State, event: gameserverTables::Event, next: gameserverTables::State, ok: bool }]
  var stages: Set[{ stage: gameserverTables::Stage, event: gameserverTables::Event, next: gameserverTables::Stage, ok: bool }]
  var decisions: Set[{ state: gameserverTables::State, stage: gameserverTables::Stage, placed: bool, reachable: bool, event: gameserverTables::Event }]

  // pkg/agent: wantOf, conditionOf and planOne.
  var wants: Set[{ state: gameserverTables::State, stage: gameserverTables::Stage, want: agentTables::Want }]
  var conditions: Set[{ sidecar: agentTables::Sidecar, game: agentTables::Game, condition: agentTables::Condition }]
  var plans: Set[{
    state: gameserverTables::State, stage: gameserverTables::Stage, sidecar: agentTables::Sidecar, game: agentTables::Game, own: bool,
    act: agentTables::Act, event: gameserverTables::Event
  }]

  // pkg/apis/scrim/v1 and pkg/nodes: Node.Duty, Node.Ask and the operations.
  var duties: Set[{ unschedulable: bool, drain: bool, duty: nodeTables::Duty }]
  var operations: Set[{
    operation: nodeTables::Operation, unschedulable: bool, drain: bool,
    after: { unschedulable: bool, drain: bool }, duty: nodeTables::Duty
  }]

  action init = all {
    transitions' = gameserverTables::transitionVectors,
    stages' = gameserverTables::stageVectors,
    decisions' = gameserverTables::decisionVectors,
    wants' = agentTables::wantVectors,
    conditions' = agentTables::conditionVectors,
    plans' = agentTables::planVectors,
    duties' = nodeTables::dutyVectors,
    operations' = nodeTables::operationVectors,
  }

  action step = all {
    transitions' = transitions, stages' = stages, decisions' = decisions,
    wants' = wants, conditions' = conditions, plans' = plans,
    duties' = duties, operations' = operations,
  }
}
