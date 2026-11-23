/-!
# Port allocation

A model of `pkg/portallocator`: the host ports a GameServer is given on a
node. The Go code keeps no table of ports. It works out which ones are taken
from the GameServers already on the node, and picks the lowest free ones.

Read `pickFree` first, then `allocate`. The theorems say what a caller may
rely on; `allocate_before_the_fix` shows the bug that the proof ran into.
-/

namespace Formal.Ports

/-- The ports of a node's range, lowest first. An empty range (`hi < lo`)
has none. -/
def range (lo hi : Nat) : List Nat :=
  List.range' lo (hi + 1 - lo)

/-- The ports of the range that nobody holds. -/
def freePorts (lo hi : Nat) (used : List Nat) : List Nat :=
  (range lo hi).filter (fun port => !used.contains port)

/-- Go: `pickFree`. The `n` lowest free ports of the range, or nothing when
the range has fewer than `n` free ports. -/
def pickFree (n lo hi : Nat) (used : List Nat) : Option (List Nat) :=
  let picked := (freePorts lo hi used).take n
  if picked.length = n then some picked else none

theorem mem_range {lo hi port : Nat} : port ∈ range lo hi ↔ lo ≤ port ∧ port ≤ hi := by
  simp [range, List.mem_range'_1]
  omega

theorem mem_freePorts {lo hi port : Nat} {used : List Nat} :
    port ∈ freePorts lo hi used ↔ (lo ≤ port ∧ port ≤ hi) ∧ port ∉ used := by
  simp [freePorts, mem_range]

theorem nodup_freePorts (lo hi : Nat) (used : List Nat) : (freePorts lo hi used).Nodup :=
  (List.nodup_range' (step := 1) (by omega)).sublist List.filter_sublist

/-- What `pickFree` returns is a prefix of the free ports. Everything else
follows from this. -/
theorem pickFree_eq {n lo hi : Nat} {used picked : List Nat}
    (h : pickFree n lo hi used = some picked) :
    picked = (freePorts lo hi used).take n ∧ picked.length = n := by
  simp only [pickFree] at h
  split at h
  · next enough => exact ⟨(Option.some.inj h).symm, (Option.some.inj h) ▸ enough⟩
  · cases h

/-- Exactly as many ports as were asked for. -/
theorem pickFree_length {n lo hi : Nat} {used picked : List Nat}
    (h : pickFree n lo hi used = some picked) : picked.length = n :=
  (pickFree_eq h).2

/-- Every port is inside the node's range and held by nobody. -/
theorem pickFree_free {n lo hi : Nat} {used picked : List Nat}
    (h : pickFree n lo hi used = some picked) :
    ∀ port ∈ picked, (lo ≤ port ∧ port ≤ hi) ∧ port ∉ used := by
  intro port hport
  rw [(pickFree_eq h).1] at hport
  exact mem_freePorts.mp (List.mem_of_mem_take hport)

/-- No port is handed out twice. -/
theorem pickFree_nodup {n lo hi : Nat} {used picked : List Nat}
    (h : pickFree n lo hi used = some picked) : picked.Nodup := by
  rw [(pickFree_eq h).1]
  exact (nodup_freePorts lo hi used).sublist (List.take_sublist _ _)

/-- It only fails when the range really has too few free ports. -/
theorem pickFree_complete {n lo hi : Nat} {used : List Nat}
    (h : n ≤ (freePorts lo hi used).length) : (pickFree n lo hi used).isSome := by
  simp [pickFree, List.length_take, Nat.min_eq_left h]

/-! ## A whole GameServer

A GameServer has several ports. Static ones name their host port themselves;
the others (Dynamic and Passthrough, which the allocator treats alike) are
given one.
-/

/-- One port of a GameServer, as far as the allocator cares. -/
inductive Port where
  | static (hostPort : Nat)
  | automatic
  deriving DecidableEq, Repr

/-- The host ports the GameServer asks for by number. -/
def staticPorts : List Port → List Nat
  | [] => []
  | .static hostPort :: rest => hostPort :: staticPorts rest
  | .automatic :: rest => staticPorts rest

/-- How many host ports have to be picked. Go: `countAutomatic`. -/
def countAutomatic : List Port → Nat
  | [] => 0
  | .static _ :: rest => countAutomatic rest
  | .automatic :: rest => countAutomatic rest + 1

/-- Go: `assign`. The host port of every port, in order: Static ports keep
theirs, the others take the picked ones one by one. -/
def assign : List Port → List Nat → List Nat
  | [], _ => []
  | .static hostPort :: rest, picked => hostPort :: assign rest picked
  | .automatic :: rest, port :: picked => port :: assign rest picked
  | .automatic :: rest, [] => assign rest []

/-- Go: `Allocate`, without the splitting of TCPUDP ports, which changes no
host port. Nothing is allocated when a Static port is taken or the range has
too few free ports. The GameServer's own Static ports count as taken while
the others are picked. -/
def allocate (ports : List Port) (lo hi : Nat) (used : List Nat) : Option (List Nat) :=
  let statics := staticPorts ports
  if statics.any used.contains then none
  else (pickFree (countAutomatic ports) lo hi (used ++ statics)).map (assign ports)

/-- `Allocate` as it was until this model was written: the picking did not
know about the GameServer's own Static ports. -/
def allocateBeforeTheFix (ports : List Port) (lo hi : Nat) (used : List Nat) : Option (List Nat) :=
  if (staticPorts ports).any used.contains then none
  else (pickFree (countAutomatic ports) lo hi used).map (assign ports)

/-- The bug: a Static port inside the node's range was handed out a second
time, to another port of the same GameServer. Docker then refuses to start
the container. -/
theorem allocateBeforeTheFix_reuses_a_static_port :
    allocateBeforeTheFix [.static 7000, .automatic] 7000 7001 [] = some [7000, 7000] := by
  decide

/-- The same request after the fix. -/
theorem allocate_skips_its_own_static_port :
    allocate [.static 7000, .automatic] 7000 7001 [] = some [7000, 7001] := by
  decide

/-- Every assigned host port is a Static one or a picked one. -/
theorem mem_assign {ports : List Port} {picked : List Nat} {port : Nat}
    (h : port ∈ assign ports picked) : port ∈ staticPorts ports ∨ port ∈ picked := by
  induction ports generalizing picked with
  | nil => simp [assign] at h
  | cons first rest ih =>
    cases first with
    | static hostPort =>
      simp only [assign, List.mem_cons] at h
      rcases h with h | h
      · exact .inl (by simp [staticPorts, h])
      · rcases ih h with h | h
        · exact .inl (by simp [staticPorts, h])
        · exact .inr h
    | automatic =>
      cases picked with
      | nil => exact (ih h).imp_left (by simp [staticPorts])
      | cons next more =>
        simp only [assign, List.mem_cons] at h
        rcases h with h | h
        · exact .inr (by simp [h])
        · exact (ih h).imp (by simp [staticPorts]) (by simp_all)

/-- Distinct Static ports and distinct picked ports that do not overlap give
distinct host ports. -/
theorem nodup_assign {ports : List Port} {picked : List Nat}
    (statics : (staticPorts ports).Nodup) (distinct : picked.Nodup)
    (apart : ∀ port ∈ staticPorts ports, port ∉ picked) :
    (assign ports picked).Nodup := by
  induction ports generalizing picked with
  | nil => simp [assign]
  | cons first rest ih =>
    cases first with
    | static hostPort =>
      simp only [staticPorts, List.nodup_cons] at statics
      simp only [staticPorts, List.mem_cons, forall_eq_or_imp] at apart
      simp only [assign, List.nodup_cons]
      refine ⟨fun h => ?_, ih statics.2 distinct apart.2⟩
      rcases mem_assign h with h | h
      · exact statics.1 h
      · exact apart.1 h
    | automatic =>
      cases picked with
      | nil => exact ih statics distinct apart
      | cons next more =>
        simp only [List.nodup_cons] at distinct
        simp only [assign, List.nodup_cons]
        refine ⟨fun h => ?_, ih statics distinct.2 fun port hp hm => apart port hp (by simp [hm])⟩
        rcases mem_assign h with h | h
        · exact apart next h (by simp)
        · exact distinct.1 h

/-- What a caller may rely on: after `allocate`, no two ports of the
GameServer share a host port, and none of them is held by another
GameServer.

The proof needs the Static ports to be distinct among themselves. The
allocator does not check that; whoever admits the GameServer has to. -/
theorem allocate_distinct {ports : List Port} {lo hi : Nat} {used hostPorts : List Nat}
    (statics : (staticPorts ports).Nodup)
    (h : allocate ports lo hi used = some hostPorts) :
    hostPorts.Nodup ∧ ∀ port ∈ hostPorts, port ∉ used := by
  simp only [allocate] at h
  split at h
  · cases h
  · next free =>
    simp only [Option.map_eq_some_iff] at h
    obtain ⟨picked, hpick, rfl⟩ := h
    have picked_free := pickFree_free hpick
    have static_free : ∀ port ∈ staticPorts ports, port ∉ used := by
      intro port hp hu
      exact free (List.any_eq_true.mpr ⟨port, hp, by simpa using hu⟩)
    refine ⟨nodup_assign statics (pickFree_nodup hpick) ?_, ?_⟩
    · intro port hp hm
      exact (picked_free port hm).2 (by simp [hp])
    · intro port hp
      rcases mem_assign hp with hp | hp
      · exact static_free port hp
      · exact fun hu => (picked_free port hp).2 (by simp [hu])

/-- Why the hypothesis is needed: two Static ports with the same number are
both "allocated". -/
theorem allocate_does_not_check_static_ports_against_each_other :
    allocate [.static 7000, .static 7000] 7000 7001 [] = some [7000, 7000] := by
  decide

end Formal.Ports
