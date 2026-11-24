/-!
# Counters

A model of what happens to a Counter when a game server changes it through
the SDK. Two pieces of Go code are involved:

* the sidecar judges the request against what it knows (`checkCounter` in
  `pkg/sdkserver/counts.go`) and answers the game server at once;
* the control plane makes the change (`counterAfter` in
  `pkg/gameservers/counts.go`), cutting the count to fit.

Read `counterAfter` and `accepted` first. The theorems then say when the two
agree, and what they silently rely on.
-/

namespace Formal.Counts

/-- A Counter of a GameServer. -/
structure Counter where
  count : Int
  capacity : Int
  deriving DecidableEq, Repr

/-- A change to a Counter: the capacity first, then the count, then the
difference. Go: `CounterUpdate`. -/
structure Update where
  capacity : Option Int := none
  count : Option Int := none
  diff : Int := 0
  deriving DecidableEq, Repr

/-- Go: `capacityAfter`. A negative capacity can only come from a broken
sender and is ignored. -/
def capacityAfter (current : Int) : Option Int → Int
  | some asked => if asked < 0 then current else asked
  | none => current

/-- The count the update asks for, before it is cut to fit. -/
def askedCount (c : Counter) (u : Update) : Int :=
  u.count.getD c.count + u.diff

/-- Go: `counterAfter`. What the control plane stores. -/
def counterAfter (c : Counter) (u : Update) : Counter :=
  let capacity := capacityAfter c.capacity u.capacity
  { count := min (max (askedCount c u) 0) capacity, capacity }

/-- Whether a count fits. Go: `checkCount`. -/
def fits (count capacity : Int) : Bool :=
  0 ≤ count && count ≤ capacity

/-- Go: `checkCounter`. Whether the sidecar accepts the request. -/
def accepted (c : Counter) (u : Update) : Bool :=
  let capacity := u.capacity.getD c.capacity
  let count := u.count.getD c.count
  (u.capacity.all (0 ≤ ·)) &&
  (u.count.all (fits · capacity)) &&
  (u.diff == 0 || fits (count + u.diff) capacity)

/-! ## What is stored stays in range -/

/-- A Counter whose capacity is not negative keeps its count between zero
and its capacity, whatever the update. -/
theorem counterAfter_in_range (c : Counter) (u : Update) (h : 0 ≤ c.capacity) :
    0 ≤ (counterAfter c u).count ∧ (counterAfter c u).count ≤ (counterAfter c u).capacity := by
  have capacity : 0 ≤ capacityAfter c.capacity u.capacity := by
    unfold capacityAfter
    split
    · split <;> omega
    · exact h
  simp only [counterAfter]
  omega

/-- The hypothesis matters: a negative capacity makes the count negative.
Nothing in `counterAfter` prevents this, so admission must never let a
Counter with a negative capacity in. -/
theorem counterAfter_with_a_negative_capacity :
    counterAfter { count := 0, capacity := -1 } {} = { count := -1, capacity := -1 } := by
  decide

/-! ## The sidecar and the control plane agree -/

/-- What the sidecar tells the game server it will be: no cutting. -/
def promised (c : Counter) (u : Update) : Counter :=
  { count := askedCount c u, capacity := u.capacity.getD c.capacity }

/-- A request the sidecar accepts is stored exactly as promised, as long as
it says something about the count. -/
theorem accepted_is_not_cut (c : Counter) (u : Update)
    (ok : accepted c u = true) (says : u.count.isSome ∨ u.diff ≠ 0) :
    counterAfter c u = promised c u := by
  obtain ⟨count, capacity⟩ := c
  obtain ⟨newCapacity, newCount, diff⟩ := u
  cases newCapacity <;> cases newCount <;>
    simp_all [accepted, fits, counterAfter, promised, askedCount, capacityAfter] <;>
    omega

/-- A request that only lowers the capacity is accepted too, and then the
count is cut: the one case where the stored count is not the asked one. -/
theorem lowering_the_capacity_cuts_the_count :
    accepted { count := 10, capacity := 100 } { capacity := some 5 } = true ∧
    counterAfter { count := 10, capacity := 100 } { capacity := some 5 } = { count := 5, capacity := 5 } := by
  decide

/-! ## 64-bit arithmetic

Go adds with `int64`, which wraps around. The model above uses unbounded
integers; this section checks that the difference cannot be observed.
-/

/-- The largest `int64`. -/
def maxInt64 : Int := 9223372036854775807

/-- Whether a number is an `int64`. -/
def isInt64 (x : Int) : Prop := -maxInt64 - 1 ≤ x ∧ x ≤ maxInt64

/-- What Go's `int64` addition gives for the exact sum `x`. -/
def wrap (x : Int) : Int :=
  (x + (maxInt64 + 1)) % (2 * (maxInt64 + 1)) - (maxInt64 + 1)

/-- The sidecar's check on a difference, computed the way Go computes it. -/
def fitsWrapped (count diff capacity : Int) : Bool :=
  fits (wrap (count + diff)) capacity

/-- If the count is not negative, a sum that wrapped around is never
accepted: the wrapped value is negative. So an accepted difference is the
exact one. -/
theorem accepted_difference_did_not_wrap (count diff capacity : Int)
    (hc : isInt64 count) (hd : isInt64 diff)
    (nonneg : 0 ≤ count) (ok : fitsWrapped count diff capacity = true) :
    wrap (count + diff) = count + diff := by
  have ⟨lower, _⟩ : 0 ≤ wrap (count + diff) ∧ wrap (count + diff) ≤ capacity := by
    simpa [fitsWrapped, fits] using ok
  simp only [wrap, isInt64, maxInt64] at *
  omega

/-- With a negative count the check can be fooled: the sum wraps around to
a large positive number that fits. Another reason admission must refuse
negative counts. -/
theorem a_negative_count_fools_the_check :
    fitsWrapped (-5) (-maxInt64 - 1) maxInt64 = true ∧
    wrap (-5 + (-maxInt64 - 1)) = maxInt64 - 4 := by
  decide

end Formal.Counts
