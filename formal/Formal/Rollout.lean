/-!
# Rolling updates

A model of one pass of a Fleet's rolling update, `rollingUpdate` in
`pkg/fleets/rollingupdate.go`: the active GameServerSet grows by the surge,
and the rest gives up the GameServers that are spare.

Read `grow` and its two theorems first: a step never takes the Fleet over
its size by more than the surge, and a step is always possible while there
is room. Then `scaleDown`. The last section asks what the floor under the
Ready GameServers is made of, and finds that it sinks.
-/

namespace Formal.Rollout

/-- What a GameServerSet says of itself. `spec` is the size it was asked to
have; the rest is its status. -/
structure Set where
  spec : Int
  replicas : Int
  ready : Int := 0
  reserved : Int := 0
  allocated : Int := 0
  shutdown : Int := 0
  deriving Repr, DecidableEq

/-! ## Growing the active set -/

/-- Go: `running`. How many GameServers a set has, or will have once it has
made the ones it is short of. -/
def running (set : Set) : Int :=
  max set.spec set.replicas

/-- Go: `grow`, given what the rest adds up to: `allocated` is what it has
Allocated, `inAll` the sum of `running` over its sets. -/
def growBy (replicas surge : Int) (active : Set) (allocated inAll : Int) : Int :=
  let room := max (replicas - allocated) 0
  if active.spec ≠ active.replicas then active.spec
  else if active.spec ≥ room then room
  else
    let asked := min (active.spec + surge) replicas
    let over := inAll + asked - (replicas + surge)
    min (if over > 0 then max (asked - over) active.spec else asked) room

/-- Go: `grow`. The size of the active set after one more step up. -/
def grow (replicas surge : Int) (active : Set) (rest : List Set) : Int :=
  growBy replicas surge active (rest.map (·.allocated)).sum (rest.map running).sum

/-- When the active set grows, the Fleet as a whole is over its size by the
surge at most. -/
theorem grow_stays_within_the_surge (replicas surge : Int) (active : Set) (allocated inAll : Int)
    (grew : active.spec < growBy replicas surge active allocated inAll) :
    inAll + growBy replicas surge active allocated inAll ≤ replicas + surge := by
  simp only [growBy] at grew ⊢
  split at grew
  · omega
  · split at grew
    · omega
    · split at grew <;> omega

/-- Being over its size never takes from the active set what it has, as long
as the sessions in the rest leave it room. A Fleet that is made smaller in
the middle of a rollout keeps its new GameServers. -/
theorem grow_keeps_what_the_active_set_has (replicas surge : Int) (active : Set) (allocated inAll : Int)
    (settled : active.spec = active.replicas)
    (surges : 0 ≤ surge) (sessions : 0 ≤ allocated)
    (room : active.spec < replicas - allocated) :
    active.spec ≤ growBy replicas surge active allocated inAll := by
  simp only [growBy]
  split
  · omega
  · split
    · omega
    · split <;> omega

/-- The active set never takes the place of an Allocated GameServer in the
rest: once its last step is done, it fits into what they leave. -/
theorem grow_leaves_room_for_sessions (replicas surge : Int) (active : Set) (allocated inAll : Int)
    (settled : active.spec = active.replicas) (fits : allocated ≤ replicas) :
    growBy replicas surge active allocated inAll + allocated ≤ replicas := by
  simp only [growBy]
  split
  · omega
  · split
    · omega
    · split <;> omega

/-- While there is room under the surge, a step makes progress. -/
theorem grow_makes_progress (replicas surge : Int) (active : Set) (allocated inAll : Int)
    (settled : active.spec = active.replicas)
    (surges : 1 ≤ surge) (sessions : 0 ≤ allocated)
    (room : active.spec < replicas - allocated)
    (under : inAll + active.spec < replicas + surge) :
    active.spec < growBy replicas surge active allocated inAll := by
  simp only [growBy]
  split
  · omega
  · split
    · omega
    · split <;> omega

/-! ## Shrinking the rest -/

/-- Go: `inUse`. The GameServers of a set that have, or are held for, a
session. No scaling down removes them. -/
def inUse (set : Set) : Int :=
  set.reserved + set.allocated

/-- Go: `dropUnhealthy`. The sizes of the rest once the GameServers nobody
can play on are taken off, `limit` at most in all. -/
def dropUnhealthy : List Set → Int → List Int
  | [], _ => []
  | set :: rest, limit =>
    let cut := min (set.spec - (set.ready + set.allocated + set.reserved)) limit
    if cut ≤ 0 then set.spec :: dropUnhealthy rest limit
    else (set.spec - cut) :: dropUnhealthy rest (limit - cut)

/-- Go: `scaleDown`. Each set comes with the size `dropUnhealthy` left it;
the answer is the new sizes, `limit` GameServers fewer at most in all. -/
def scaleDown : List (Set × Int) → Int → List Int
  | [], _ => []
  | (set, size) :: rest, limit =>
    if limit ≤ 0 then size :: rest.map (·.2)
    else if size ≤ 0 then size :: scaleDown rest limit
    else if inUse set ≥ size then 0 :: scaleDown rest limit
    else if size ≠ set.replicas then size :: rest.map (·.2)
    else if set.shutdown ≠ 0 then size :: scaleDown rest limit
    else (size - min size limit) :: scaleDown rest (limit - min size limit)

/-- How many GameServers a set gives up when its size is lowered. The ones
in use stay whatever the size says: that is `scaling_down_spares_sessions`
in the state machine. So a set gives up no more than it has idle. -/
def givenUp (set : Set) (size new : Int) : Int :=
  min (size - new) (max (size - inUse set) 0)

def totalGivenUp : List (Set × Int) → List Int → Int
  | (set, size) :: rest, new :: news => givenUp set size new + totalGivenUp rest news
  | _, _ => 0

theorem nothing_is_given_up_by_staying (sets : List (Set × Int)) :
    totalGivenUp sets (sets.map (·.2)) = 0 := by
  induction sets with
  | nil => rfl
  | cons head rest ih =>
    obtain ⟨set, size⟩ := head
    simp only [List.map, totalGivenUp, givenUp, ih]
    omega

/-- `scaleDown` takes away `limit` GameServers at most. Calling a set done
that holds nothing but sessions takes away none. -/
theorem scaleDown_gives_up_the_limit_at_most (sets : List (Set × Int)) (limit : Int) :
    totalGivenUp sets (scaleDown sets limit) ≤ max limit 0 := by
  induction sets generalizing limit with
  | nil => simp only [totalGivenUp]; omega
  | cons head rest ih =>
    obtain ⟨set, size⟩ := head
    have stay := nothing_is_given_up_by_staying rest
    have same := ih limit
    have less := ih (limit - min size limit)
    simp only [scaleDown]
    split
    · simp only [totalGivenUp, givenUp, stay]; omega
    · split
      · simp only [totalGivenUp, givenUp]; omega
      · split
        · simp only [totalGivenUp, givenUp]; omega
        · split
          · simp only [totalGivenUp, givenUp, stay]; omega
          · split
            · simp only [totalGivenUp, givenUp]; omega
            · simp only [totalGivenUp, givenUp]; omega

/-! ## The floor under the Ready GameServers -/

/-- Go: `maxUnavailable` for a percentage. It is taken of the GameServers
that were Ready at the last pass, rounded down; it is at least 1 and at most
the Fleet's size. -/
def unavailable (replicas ready percent : Int) : Int :=
  min (max (ready * percent / 100) 1) replicas

/-- Go: `minAvailable` in `shrink`. How many Ready GameServers a pass leaves. -/
def minAvailable (replicas ready percent : Int) : Int :=
  min (ready - unavailable replicas ready percent) replicas

/-- Go: `missing`. How many GameServers the active set is waiting for. -/
def missing (active : Set) : Int :=
  active.spec - active.ready - active.reserved - active.allocated - active.shutdown

/-- Go: `shrink`. The new sizes of the rest. `fleetReady` is the Fleet's
count of Ready GameServers, as its status had it at the last pass. -/
def shrink (replicas percent fleetReady : Int) (active : Set) (rest : List Set) : List Int :=
  let ready := active.ready + (rest.map (·.ready)).sum
  let spare := ready - minAvailable replicas fleetReady percent
  let most := spare - missing active
  if most ≤ 0 then rest.map (·.spec)
  else scaleDown (rest.zip (dropUnhealthy rest most)) spare

/-- The floor is measured from what is Ready now, not from the size of the
Fleet. So it is always below what is Ready: every pass may take some. -/
theorem the_floor_is_below_what_is_ready (replicas ready percent : Int) (sized : 1 ≤ replicas) :
    minAvailable replicas ready percent < ready := by
  simp only [minAvailable, unavailable]
  omega

/-- A Fleet in the middle of a rollout: an old set whose GameServers are all
Ready, and a new set whose GameServers never get there. Every status has
caught up with its spec. -/
structure Stuck where
  old : Int
  new : Int
  deriving Repr, DecidableEq

/-- One pass over such a Fleet: `rollingUpdate` in Go. -/
def Stuck.pass (replicas percent surge : Int) (s : Stuck) : Stuck :=
  let active : Set := { spec := s.new, replicas := s.new }
  let rest : Set := { spec := s.old, replicas := s.old, ready := s.old }
  { new := grow replicas surge active [rest]
    old := (shrink replicas percent s.old active [rest]).headD s.old }

/-- The state after each of the next passes. -/
def Stuck.passes (replicas percent surge : Int) : Nat → Stuck → List Stuck
  | 0, _ => []
  | n + 1, s =>
    let next := s.pass replicas percent surge
    next :: Stuck.passes replicas percent surge n next

/-- A Fleet of 100 with maxUnavailable 25% and maxSurge 1 is given a version
that never becomes Ready. The floor follows the Ready GameServers down: 75,
57, 43 and so on. The rollout only stops taking them when the new set waits
for more GameServers than a pass may take, and by then 19 of the 100 are
left. A floor measured from the Fleet's size would have held at 75. -/
theorem a_version_that_never_gets_ready_takes_most_of_the_fleet :
    (Stuck.passes 100 25 1 8 { old := 100, new := 0 }).map (·.old)
      = [75, 57, 43, 33, 25, 19, 19, 19] := by
  decide

end Formal.Rollout
