import Formal.Ports
import Formal.Counts
import Formal.StateMachine
import Formal.Rollout

/-!
# Test vectors

The models are only worth something if the Go code behaves like them. This
program runs the models on many small inputs and writes down what they
answer, one JSON object per line. Go tests replay the files against the real
functions (`go test ./pkg/portallocator ./pkg/gameservers ./pkg/sdkserver
./pkg/fleets`).

Run `lake exe vectors` in this directory after changing a model.
-/

open Formal

/-- Every list of exactly `n` letters. -/
def words {α : Type} (alphabet : List α) : Nat → List (List α)
  | 0 => [[]]
  | n + 1 => (words alphabet n).flatMap fun word => alphabet.map (· :: word)

/-- Every list of at most `n` letters. -/
def wordsUpTo {α : Type} (alphabet : List α) (n : Nat) : List (List α) :=
  (List.range (n + 1)).flatMap (words alphabet)

/-- Every subset, as a list. -/
def subsets {α : Type} : List α → List (List α)
  | [] => [[]]
  | x :: rest => (subsets rest).flatMap fun s => [s, x :: s]

def jsonList (items : List String) : String :=
  "[" ++ ",".intercalate items ++ "]"

def jsonOption {α : Type} (show_ : α → String) : Option α → String
  | some x => show_ x
  | none => "null"

/-! ## Ports -/

def jsonPort : Ports.Port → String
  | .static hostPort => "{\"static\":" ++ toString hostPort ++ "}"
  | .automatic => "\"automatic\""

def jsonNats (xs : List Nat) : String :=
  jsonList (xs.map toString)

def portVector (ports : List Ports.Port) (range : Nat × Nat) (used : List Nat) : String :=
  "{\"ports\":" ++ jsonList (ports.map jsonPort) ++
  ",\"lo\":" ++ toString range.1 ++ ",\"hi\":" ++ toString range.2 ++
  ",\"used\":" ++ jsonNats used ++
  ",\"hostPorts\":" ++ jsonOption jsonNats (Ports.allocate ports range.1 range.2 used) ++ "}"

/-- Up to three ports, inside, at the edge of and outside small ranges. -/
def portVectors : List String :=
  let alphabet := [.static 7000, .static 7001, .static 7005, .automatic]
  let ranges := [(7000, 7002), (7001, 7001), (7002, 7000)]
  (wordsUpTo alphabet 3).flatMap fun ports =>
    ranges.flatMap fun range =>
      (subsets [7000, 7001, 7002]).map fun used => portVector ports range used

/-! ## Counters -/

def jsonInt (x : Int) : String := toString x

def counterVector (c : Counts.Counter) (u : Counts.Update) : String :=
  let after := Counts.counterAfter c u
  "{\"count\":" ++ jsonInt c.count ++ ",\"capacity\":" ++ jsonInt c.capacity ++
  ",\"setCapacity\":" ++ jsonOption jsonInt u.capacity ++
  ",\"setCount\":" ++ jsonOption jsonInt u.count ++
  ",\"diff\":" ++ jsonInt u.diff ++
  ",\"accepted\":" ++ toString (Counts.accepted c u) ++
  ",\"afterCount\":" ++ jsonInt after.count ++ ",\"afterCapacity\":" ++ jsonInt after.capacity ++ "}"

/-- Counters and updates around the interesting edges: zero, the capacity,
one past it, and values that should never occur. -/
def counterVectors : List String :=
  let counts : List Int := [-1, 0, 1, 5, 10]
  let capacities : List Int := [-1, 0, 5, 10]
  let newCapacities : List (Option Int) := [none, some (-1), some 0, some 3, some 10]
  let newCounts : List (Option Int) := [none, some (-1), some 0, some 4, some 11]
  let diffs : List Int := [-11, -1, 0, 1, 11]
  counts.flatMap fun count => capacities.flatMap fun capacity =>
    newCapacities.flatMap fun newCapacity => newCounts.flatMap fun newCount =>
      diffs.map fun diff =>
        counterVector { count, capacity } { capacity := newCapacity, count := newCount, diff }

/-! ## The state machine -/

/-- The name Go uses for a state. -/
def stateName : StateMachine.State → String
  | .portAllocation => "PortAllocation" | .creating => "Creating"
  | .starting => "Starting" | .scheduled => "Scheduled"
  | .requestReady => "RequestReady" | .ready => "Ready"
  | .reserved => "Reserved" | .allocated => "Allocated"
  | .shutdown => "Shutdown" | .unhealthy => "Unhealthy" | .error => "Error"

/-- The name Go uses for an event. -/
def eventName : StateMachine.Event → String
  | .placed => "Placed" | .readied => "Readied" | .scaledDown => "ScaledDown"
  | .nodeLost => "NodeLost" | .containersCreated => "ContainersCreated"
  | .createFailed => "CreateFailed" | .containersRunning => "ContainersRunning"
  | .exitedCleanly => "ExitedCleanly" | .exitedWithError => "ExitedWithError"
  | .containersMissing => "ContainersMissing" | .containersRemoved => "ContainersRemoved"
  | .sdkReady => "SDKReady" | .sdkAllocate => "SDKAllocate" | .sdkReserve => "SDKReserve"
  | .sdkShutdown => "SDKShutdown" | .healthFailed => "HealthFailed"
  | .reserveExpired => "ReserveExpired"

def jsonString (text : String) : String := "\"" ++ text ++ "\""

/-- The whole table: every state with every event. Since nothing is left
out, a Go test that replays this shows the model and `Next` to be equal. -/
def transitionVectors : List String :=
  StateMachine.State.all.flatMap fun state =>
    StateMachine.Event.all.map fun event =>
      "{\"state\":" ++ jsonString (stateName state) ++
      ",\"event\":" ++ jsonString (eventName event) ++
      ",\"next\":" ++ jsonOption (jsonString ∘ stateName) (StateMachine.next state event) ++ "}"

/-! ## Rolling updates -/

def jsonSet (s : Rollout.Set) : String :=
  "{\"spec\":" ++ jsonInt s.spec ++ ",\"replicas\":" ++ jsonInt s.replicas ++
  ",\"ready\":" ++ jsonInt s.ready ++ ",\"reserved\":" ++ jsonInt s.reserved ++
  ",\"allocated\":" ++ jsonInt s.allocated ++ ",\"shutdown\":" ++ jsonInt s.shutdown ++ "}"

/-- One pass over a Fleet: what `grow` and `shrink` answer. `stale` is how
far the Fleet's own count of Ready GameServers is ahead of its sets'. -/
def rolloutVector (replicas surge percent stale : Int) (active : Rollout.Set) (rest : List Rollout.Set) : String :=
  let fleetReady := active.ready + (rest.map (·.ready)).sum + stale
  "{\"replicas\":" ++ jsonInt replicas ++ ",\"surge\":" ++ jsonInt surge ++
  ",\"percent\":" ++ jsonInt percent ++ ",\"fleetReady\":" ++ jsonInt fleetReady ++
  ",\"active\":" ++ jsonSet active ++ ",\"rest\":" ++ jsonList (rest.map jsonSet) ++
  ",\"grow\":" ++ jsonInt (Rollout.grow replicas surge active rest) ++
  ",\"shrink\":" ++ jsonList ((Rollout.shrink replicas percent fleetReady active rest).map jsonInt) ++ "}"

/-- Sets in the shapes a rollout meets: full, part of it unhealthy, short
of its size, in the middle of a step, and holding sessions in various ways. -/
def rolloutVectors : List String :=
  let actives : List Rollout.Set := [
    { spec := 0, replicas := 0 },
    { spec := 2, replicas := 2, ready := 2 },
    { spec := 2, replicas := 2 },
    { spec := 3, replicas := 2, ready := 2 }]
  let olds : List Rollout.Set := [
    { spec := 4, replicas := 4, ready := 4 },
    { spec := 4, replicas := 4, ready := 2 },
    { spec := 2, replicas := 4, ready := 1, allocated := 3 },
    { spec := 3, replicas := 3, ready := 1, reserved := 1, allocated := 1 },
    { spec := 2, replicas := 0 }]
  [3, 6].flatMap fun replicas => [1, 2].flatMap fun surge =>
    [25, 100].flatMap fun percent => [0, 2].flatMap fun stale =>
      actives.flatMap fun active =>
        (wordsUpTo olds 2).map fun rest => rolloutVector replicas surge percent stale active rest

/-- The rollout of `a_version_that_never_gets_ready_takes_most_of_the_fleet`:
how many Ready GameServers the old set has after each pass. -/
def stuckVectors : List String :=
  let passes := Rollout.Stuck.passes 100 25 1 8 { old := 100, new := 0 }
  ["{\"replicas\":100,\"percent\":25,\"surge\":1,\"old\":" ++ jsonList (passes.map (jsonInt ·.old)) ++
   ",\"new\":" ++ jsonList (passes.map (jsonInt ·.new)) ++ "}"]

def write (file : String) (lines : List String) : IO Unit := do
  IO.FS.writeFile file ("\n".intercalate lines ++ "\n")
  IO.println s!"{file}: {lines.length} vectors"

def main : IO Unit := do
  write "vectors/ports.jsonl" portVectors
  write "vectors/counters.jsonl" counterVectors
  write "vectors/transitions.jsonl" transitionVectors
  write "vectors/rollout.jsonl" rolloutVectors
  write "vectors/stuck.jsonl" stuckVectors
