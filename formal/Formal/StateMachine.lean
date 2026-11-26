/-!
# The life of a GameServer

A model of `pkg/gameservers/state.go`: which state a GameServer moves to
when something happens to it. The table is small enough to check every
combination, so each theorem here is settled by trying them all.

Read `rule` first: it is the Go table, row for row.
-/

namespace Formal.StateMachine

/-- The states of a GameServer. Go: `agonesv1.GameServerState`. -/
inductive State where
  | portAllocation | creating | starting | scheduled | requestReady | ready
  | reserved | allocated | shutdown | unhealthy | error
  deriving DecidableEq, Repr

/-- What can happen to a GameServer. Go: `Event`. -/
inductive Event where
  -- reported by the control plane itself
  | placed | readied | allocatedByRequest | scaledDown | nodeLost
  -- reported by the node's agent
  | containersCreated | createFailed | containersRunning
  | exitedCleanly | exitedWithError | containersMissing | containersRemoved
  -- reported by the sidecar
  | sdkReady | sdkAllocate | sdkReserve | sdkShutdown | healthFailed | reserveExpired
  deriving DecidableEq, Repr

open State Event

def State.all : List State :=
  [portAllocation, creating, starting, scheduled, requestReady, ready,
   reserved, allocated, shutdown, unhealthy, error]

def Event.all : List Event :=
  [placed, readied, allocatedByRequest, scaledDown, nodeLost, containersCreated, createFailed,
   containersRunning, exitedCleanly, exitedWithError, containersMissing,
   containersRemoved, sdkReady, sdkAllocate, sdkReserve, sdkShutdown, healthFailed,
   reserveExpired]

/-- Go: `Final`. A state a GameServer never leaves. -/
def final : State → Bool
  | shutdown | unhealthy | error => true
  | _ => false

/-- Go: `beforeSession`. No players are on the server, or about to be. -/
def beforeSession : List State :=
  [portAllocation, creating, starting, scheduled, requestReady, ready]

/-- One row of the table: the states the event applies in, and where it
leads. `anywhere` means every state that is not final. -/
structure Rule where
  sources : Option (List State)
  target : State

def anywhere (target : State) : Rule := { sources := none, target }
def only (sources : List State) (target : State) : Rule := { sources := some sources, target }

/-- Go: `rules`. An event without a row never changes the state. -/
def rule : Event → Option Rule
  | placed => some (only [portAllocation, creating] creating)
  | containersCreated => some (only [creating] starting)
  | createFailed => some (only [creating, starting] error)
  | containersRunning => some (only [starting] scheduled)
  | readied => some (only [requestReady] ready)
  | allocatedByRequest => some (only [ready, allocated] allocated)
  | scaledDown => some (only beforeSession shutdown)
  | sdkReady => some (anywhere requestReady)
  | sdkAllocate => some (anywhere allocated)
  | sdkReserve => some (anywhere reserved)
  | sdkShutdown => some (anywhere shutdown)
  | reserveExpired => some (only [reserved] requestReady)
  | exitedCleanly => some (anywhere shutdown)
  | exitedWithError => some (anywhere unhealthy)
  | containersMissing => some (anywhere unhealthy)
  | healthFailed => some (anywhere unhealthy)
  | nodeLost => some (anywhere unhealthy)
  | containersRemoved => none

/-- Go: `Next`. The state after an event, or nothing when the event does not
apply, so that late and repeated events are ignored. -/
def next (state : State) (event : Event) : Option State :=
  match rule event with
  | none => none
  | some row =>
    if final state then none
    else match row.sources with
      | none => some row.target
      | some sources => if sources.contains state then some row.target else none

/-- The state after a series of events, ignoring those that do not apply. -/
def run (state : State) : List Event → State
  | [] => state
  | event :: rest => run ((next state event).getD state) rest

/-! ## What the table promises -/

/-- A final state is final: nothing moves a GameServer out of it. -/
theorem final_is_final (state : State) (event : Event) (h : final state = true) :
    next state event = none := by
  cases state <;> cases event <;> simp_all [final, next, rule]

/-- Scaling down never takes a GameServer that has, or is promised, a
session. -/
theorem scaling_down_spares_sessions (state : State) (h : state = allocated ∨ state = reserved) :
    next state scaledDown = none := by
  rcases h with rfl | rfl <;> decide

/-- Only two kinds of event end a session: the game server's own requests,
and failures. The control plane's routine never does. -/
theorem what_ends_a_session (event : Event) (after : State)
    (h : next allocated event = some after) (changed : after ≠ allocated) :
    event ∈ [sdkReady, sdkReserve, sdkShutdown,
             exitedCleanly, exitedWithError, containersMissing, healthFailed, nodeLost] := by
  cases event <;> simp_all [next, rule, final, anywhere, only, beforeSession]

/-- A reservation that runs out takes a GameServer back only while it is
still Reserved. The sidecar's timer may fire just as the game server
allocates itself; what the timer says then arrives too late to matter. -/
theorem an_expired_reservation_ends_only_a_reservation (state after : State)
    (h : next state reserveExpired = some after) :
    state = reserved ∧ after = requestReady := by
  cases state <;> simp_all [next, rule, final, only]

/-- A matchmaker is given a GameServer only while it waits for a session, or
has one already. One that is still coming up, held back by its game server,
or finished is never handed out, whatever the allocation API is asked. -/
theorem a_request_gets_only_what_can_take_a_session (state after : State)
    (h : next state allocatedByRequest = some after) :
    (state = ready ∨ state = allocated) ∧ after = allocated := by
  cases state <;> simp_all [next, rule, final, only]

/-- No state is a trap: a GameServer that is not finished can always be
shut down, and can always be declared lost. -/
theorem never_stuck (state : State) (h : final state = false) :
    next state sdkShutdown = some shutdown ∧ next state nodeLost = some unhealthy := by
  cases state <;> simp_all [final, next, rule, anywhere]

/-- Every state can be reached from the first one, so no row is dead. -/
theorem every_state_is_reachable (state : State) :
    ∃ events, run portAllocation events = state := by
  cases state
  · exact ⟨[], rfl⟩
  · exact ⟨[placed], rfl⟩
  · exact ⟨[placed, containersCreated], rfl⟩
  · exact ⟨[placed, containersCreated, containersRunning], rfl⟩
  · exact ⟨[placed, containersCreated, containersRunning, sdkReady], rfl⟩
  · exact ⟨[placed, containersCreated, containersRunning, sdkReady, readied], rfl⟩
  · exact ⟨[placed, containersCreated, containersRunning, sdkReserve], rfl⟩
  · exact ⟨[placed, containersCreated, containersRunning, sdkAllocate], rfl⟩
  · exact ⟨[sdkShutdown], rfl⟩
  · exact ⟨[nodeLost], rfl⟩
  · exact ⟨[placed, createFailed], rfl⟩

/-- The ordinary life, from creation to the end of a session. -/
theorem the_ordinary_life :
    run portAllocation
      [placed, containersCreated, containersRunning, sdkReady, readied, sdkAllocate, sdkShutdown]
    = shutdown := by
  decide

/-- A game server may ask for a session before the agent has reported its
containers running. The table allows it, and `containersRunning` then no
longer applies. Whatever the controller does on `containersRunning` (such as
publishing the address) must therefore not depend on the state changing. -/
theorem allocated_before_running :
    run portAllocation [placed, containersCreated, sdkAllocate] = allocated ∧
    next allocated containersRunning = none := by
  decide

end Formal.StateMachine
