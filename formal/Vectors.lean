import Formal.Ports
import Formal.Counts
import Formal.StateMachine

/-!
# Test vectors

The models are only worth something if the Go code behaves like them. This
program runs the models on many small inputs and writes down what they
answer, one JSON object per line. Go tests replay the files against the real
functions (`go test ./pkg/portallocator ./pkg/gameservers ./pkg/sdkserver`).

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

def write (file : String) (lines : List String) : IO Unit := do
  IO.FS.writeFile file ("\n".intercalate lines ++ "\n")
  IO.println s!"{file}: {lines.length} vectors"

def main : IO Unit := do
  write "vectors/ports.jsonl" portVectors
  write "vectors/counters.jsonl" counterVectors
  write "vectors/transitions.jsonl" transitionVectors
