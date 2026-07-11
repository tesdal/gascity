# Phase 6 Handoff — CachingStore ConditionalWriter: forward-and-EVICT, never patch (S2-T8, the livelock MERGE GATE)

Pick-up doc for the next session. **PR-S2a Phases 1–5 are complete** (interface,
typed errors, conformance harness, MemStore + FileStore native CAS, BdStore
classifier + probe + the three `*IfMatch` verbs + `runConditionalWrite` +
metadata-CAS emulation, and the F2 Doltlite loud-degrade — `ga-zj78gu` CLOSED).
This hands off **Phase 6 (S2-T8)**: CachingStore forwards the four CAS methods to
its backing store and maintains the cache by **evicting, never patching** — plus
the **merge-gate regression test** `TestCachingStoreCASRetryLoopConverges`
(DESIGN §8.5 makes it a merge gate of the stage-2 PR, not a follow-up). All still
**INERT** — zero consumers resolve `ConditionalWriterFor` on a CachingStore.

The kickoff prompt is `PHASE6-PROMPT.md` (paste it to start). This doc is the
grounding; the build spec is `PR-S2a-BUILD-SPEC.md` (keep its Progress block
current — it now carries the Phase-5 entry and the dated S2-T7 SQL-spike verdict).
DESIGN references below mean `engdocs/plans/feature-flags/DESIGN.md` (§8.5 =
"CachingStore: forward, and evict — never patch"). **All bare `*.go` filenames in
this doc live in `internal/beads/`** (the two production construction sites are
the only fully-qualified exceptions).

---

## Status — what's committed (branch `worktree-reconciler`, local/UNPUSHED)

| Commit | Task | Content |
|--------|------|---------|
| `44eb2ab70` | docs | `PR-S2a-BUILD-SPEC.md` |
| `bec9156b1` | S2-T1 | `ConditionalWriter` interface + revision/granularity contract; 4 typed errors + `IsX` helpers; `Bead.Revision int64 json:"-"`; `ConditionalWriterFor`/`ConditionalWriterHandleProvider` |
| `ec0bccd04` | S2-T2, T3-mem | `beadstest/conditional_writer_conformance.go` harness; MemStore native CAS + `DisableConditionalWrites` |
| `da0d073a6` | S2-T3-file | FileStore native CAS (flock-wrapped) + out-of-band `fileData.Revisions` |
| `6c0160669` | S2-T4/T5 | BdStore classifier + four-verb capability probe + latch (`bdstore_conditional.go`) |
| `3f113a52a` | **S2-T6/T7 + F2** | BdStore `*IfMatch` verbs + `runConditionalWrite` retry wrapper + `finalizeConditionalWrite` + `CompareAndSetMetadataKey` emulation + `var _ ConditionalWriter = (*BdStore)(nil)`; `bdUpdateArgs` extraction; **DoltliteReadStore loud-degrade shadows (ga-zj78gu CLOSED)**; SQL-spike verdict recorded (emulation ships, SQL dropped) |

**Do not push** (local integration stack). **Two untracked dirs are NOT mine** —
`engdocs/plans/beads-cas/`, `engdocs/plans/reconciler-redesign/`. Leave them; never `git add` them.

## Phase 1–5 API surface Phase 6 consumes

- `ConditionalWriter` (beads.go:168-186) — the four methods CachingStore must now
  implement. `ConditionalWriterFor(store)` (beads.go:202) — use it on `c.backing`
  to resolve the backing writer (direct assert → `ConditionalWriterHandleProvider`
  → `(nil,false)`). When it returns false, the CachingStore verb returns
  `ErrConditionalWriteUnsupported` (and `(false, ErrConditionalWriteUnsupported)`
  for CAS) — the exact `ReleaseIfCurrent` template shape
  (caching_store_writes.go:138-141 with `ErrConditionalReleaseUnsupported`).
- Typed error structs + helpers (beads.go:215-294: `PreconditionFailedError`
  :215-233, `IsPreconditionFailed` :235-239, `GateRefusalError` :241-263,
  `CASRetriesExhaustedError` :265-288, `IsConditionalWriteUnsupported` :290-294;
  the `ErrConditionalWriteUnsupported` sentinel is at beads.go:40-44). The
  **evict trigger** is `IsPreconditionFailed(err)` on the error the backing
  writer returned — BdStore stamps `ID`/`Expected` in `finalizeConditionalWrite`;
  MemStore/FileStore stamp natively. Do NOT re-stamp in CachingStore; forward
  the error untouched.
- MemStore native CAS + `DisableConditionalWrites` (memstore.go) — the conformance
  backing. MemStore supplies `Current` (its conformance row sets
  `SuppliesCurrent: true`, memstore_test.go:23-33), and CachingStore forwards
  errors untouched → the CachingStore-over-MemStore row can also set
  `SuppliesCurrent: true`.
- Harness `beadstest.RunConditionalWriterConformanceWithOptions` — already
  **CachingStore-aware**: the close-leg tolerates a store whose `Get` on a closed
  bead returns ErrNotFound (conditional_writer_conformance.go:112-124), and
  `Expected` is asserted unconditionally (:219).
- Production CachingStore constructions (still inert, for awareness only):
  `cmd/gc/api_state.go:234` and `internal/runtime/t3bridge/provider.go:1700` wrap
  a bd-backed store — once Phase 6 lands, `ConditionalWriterFor(cachingStore)`
  will assert true there, but nothing calls it yet.

## Phase 6 build task (S2-T8) — DESIGN §8.5 verbatim, build spec "CachingStore forward-and-EVICT"

### The rule: forward, and evict — never patch
The existing write paths refresh after a successful write and, when the refresh
fails, **optimistically patch** the cached clone (`Update` else-branch
caching_store_writes.go:97-111, `ReleaseIfCurrent` :160-171, `SetMetadata`
:361-370, `SetMetadataBatch` :413-424). A CAS port of that fallback is poison:
the local patch cannot synthesize the new revision, `CachingStore.Get` serves the
cached clone, and every consumer's precondition recovery re-reads the **stale**
revision through the cache and re-fails — a livelock indistinguishable from real
contention. Therefore:

- CAS success + successful refresh → refresh the cache entry (normal path —
  reuse `refreshBeadAfterWrite`, caching_store_writes.go:437-444, which reads
  `c.backing.Get` and `recordProblem`s on failure).
- CAS success + **failed** refresh → **EVICT**: remove the entry so the next
  `Get` goes to the backing store (see G1/G2 below for the exact mechanics the
  `Get` path dictates).
  **`CloseIfMatch` carve-out (pin in the design pass, D1/D2):** a blanket
  refresh-fail rule mis-handles ErrNotFound after a SUCCESSFUL close — the
  unconditional `Close` deliberately TOLERATES ErrNotFound from the post-close
  backing `Get` (caching_store_writes.go:195-203: no `recordProblem`, synthesizes
  the closed clone from cache). MemStore's `Get` returns closed beads so the
  conformance row won't expose this, but a production bd backing that hides
  closed beads from `Get` would turn every successful `CloseIfMatch` into
  refresh-failure noise + evict. Treat close-then-ErrNotFound as tolerated (evict
  silently or synthesize the closed clone — pick one), not as a recordProblem
  refresh failure. `DeleteIfMatch` success never refreshes at all (the bead is
  gone by definition) — it goes straight to the `Delete` scrub below.
- **Every `*PreconditionFailedError` from the backing writer → evict too.** The
  cached revision is proven stale by construction; keeping it feeds the caller's
  re-read the same dead revision.
- `DeleteIfMatch` success → mirror `Delete`'s full scrub (caching_store_writes.go:890-912:
  delete beads/deps/dirty/beadSeq/localBeadAt + **`deletedSeq[id]=seq`** +
  `clearDependentReadyProjectionsLocked` + notify `bead.deleted`). The
  `deletedSeq` stamp is correct HERE and only here — the bead is actually gone.
- Never add the idempotence short-circuits to the fenced paths (G3).
- Compile assert `var _ ConditionalWriter = (*CachingStore)(nil)` beside the
  existing asserts (caching_store.go:84-85) or in the new file.

### The MERGE GATE test — `TestCachingStoreCASRetryLoopConverges`
CAS succeeds, the post-write refresh is forced to fail once → the entry is
EVICTED → the next `Get` hits the backing store and returns the post-write state
(new revision) → a retry with that revision **succeeds** (converges, not
livelocks). Plus the second leg: a stale-cache `UpdateIfMatch` (backing mutated
out-of-band) → backing returns `PreconditionFailedError` → surfaces to the caller
AND the entry is evicted → the caller's re-read through the cache gets the fresh
backing revision → retry succeeds.

**Anti-vacuity requirement:** prove the pre-evict reads were cache-served (a
counting backing wrapper: assert backing `Get` calls did NOT increase on a
cache-served read, then DID after eviction) — otherwise "next Get hits backing"
is vacuously true on an unprimed cache (G6).

### Conformance row
Run `beadstest.RunConditionalWriterConformanceWithOptions(t, "CachingStore", open, ...)`
over `NewCachingStoreForTest(NewMemStore(), nil)` + `Prime(ctx)` (Prime at
caching_store.go:450; `NewCachingStoreForTest` :263). Options: `SuppliesCurrent:
true` (forwarded MemStore errors carry Current); `OpenDisabled`: CachingStore over
a MemStore with `DisableConditionalWrites = true` — the backing still CLAIMS the
interface, so `ConditionalWriterFor(c.backing)` resolves and the backing returns
the typed unsupported per call, which CachingStore forwards. ALSO test the
capability-absent leg separately: CachingStore over a backing that does NOT
implement ConditionalWriter at all (an interface-embedding wrapper strips the
methods naturally — see G4) → all four verbs return typed unsupported directly
from the `ConditionalWriterFor(c.backing)` miss.

---

## CachingStore surface map (verified file:line — don't re-explore)

- **Struct** caching_store.go:28 — `backing Store` is an interface **FIELD, not
  embedded** → no F2-style promotion hazard; nothing in the repo embeds
  `*CachingStore` either (verified by grep). The compile asserts block is :84-85.
- **`Get`** caching_store_reads.go:384-446 — the semantics that dictate the evict
  design (G1/G2): `deletedSeq[id]` set → **hard ErrNotFound WITHOUT consulting
  backing** (:386-389); recent-local-mutation guard (:390-397) serves the cached
  clone when `beadSeq` set ∧ not dirty ∧ present; in live/partial state
  `dirty[id]` → **backing.Get + re-prime the entry** (:399-435); plain cache miss
  → backing.Get fallthrough without re-prime (:437-442); cold state → backing
  passthrough (:444-445).
- **Write-path bookkeeping idiom** (mirror on success+refresh): Update
  caching_store_writes.go:119-133 — `noteLocalMutationLocked` (:358 in
  caching_store.go), `beads[id]=cloneBead(fresh)`, `deps[id]=depsFromBeadFields`,
  `clearDependentReadyProjectionsLocked` when status changed
  (caching_store_events.go:384), `delete(dirty,id)`, `delete(deletedSeq,id)`,
  `markFreshLocked` (caching_store.go:826), `updateStatsLocked` (:875),
  `notifyChange` (caching_store_events.go:648).
- **The optimistic-patch else-branches to NOT copy:** Update :97-111,
  ReleaseIfCurrent :160-171, SetMetadata :361-370, SetMetadataBatch :413-424.
- **`ReleaseIfCurrent` capability-forward template:** :138-146 (type-assert
  backing → typed unsupported when missing → forward → maintain cache only on
  success).
- **`GraphApplyHandle`** caching_store_graph_apply.go:12-24 — the OTHER
  capability-forward precedent (a handle wrapping `GraphApplyFor(c.backing)`).
  Phase 6 does NOT need a handle type: the compile assert requires methods on
  `*CachingStore` directly; resolve the backing writer per call.
- **`Delete` full-scrub precedent** :890-912 (for DeleteIfMatch success).
- **Idempotence short-circuits to keep OFF the fenced paths:**
  `updateMatchesCached` :650, `closeAlreadyMatchesCached` :727,
  `metadataAlreadyMatchesCached` :752.
- **Test scaffolding precedents** (caching_store_writes_internal_test.go):
  `countingBackingStore` :13 (embeds the `Store` interface, counts calls, and
  re-implements `ReleaseIfCurrent` :42-49 — the exact dance G4 requires),
  `releaseRefreshFailOnceStore` :63-86 (fails the next `Get` once after a
  successful conditional release — the fail-refresh-once shape the merge gate
  needs, extended to CAS).
- **`cloneBead`** memstore.go:71 — value copy + deep-copied reference fields;
  `Revision` (plain int64) survives → the cache serves revisions transparently
  and refresh-after-write keeps them current.

## Deep gotchas (each will bite; all verified against source)

- **G1 — `deletedSeq` is poison for evict.** `Get` returns ErrNotFound WITHOUT
  consulting backing whenever `deletedSeq[id]` is set (reads.go:386-389). An
  evict-because-refresh-failed or evict-on-precondition MUST NOT stamp
  `deletedSeq` — the bead still exists; stamping it makes every subsequent read
  a false ErrNotFound and the merge-gate test can never converge. Only
  `DeleteIfMatch` SUCCESS stamps it (mirroring `Delete`).
- **G2 — `dirty` is the evict vehicle.** `dirty[id]` in live/partial state sends
  the next `Get` to backing AND re-primes the entry (:399-435); a plain map miss
  also reaches backing but without re-prime (:437-442). The existing
  refresh-failure branches set `c.dirty[id] = struct{}{}`. Evict ≈
  `delete(c.beads,id)` + `delete(c.deps,id)` + `c.dirty[id]=struct{}{}` +
  `clearDependentReadyProjectionsLocked(id)` + `noteLocalMutationLocked(id)` +
  `markFreshLocked`/`updateStatsLocked` — the exact composition is design-pass
  item D1, but dirty-not-deletedSeq is settled by the `Get` semantics above.
- **G3 — no idempotence short-circuits on fenced paths.** Only the backing can
  evaluate the fence. A cached value-match proves nothing about the revision; a
  short-circuit returning nil fabricates a CAS success without a revision bump
  (the conformance `every_mutation_bumps`/`revision_monotonic` legs are the
  teeth). Also do NOT pre-check the cached metadata value in
  `CompareAndSetMetadataKey` — forward directly.
- **G4 — the test-fake promotion trap.** The existing backing-wrapper fakes embed
  the `Store` INTERFACE (`countingBackingStore`, `releaseRefreshFailOnceStore`) —
  interface embedding does NOT promote MemStore's CAS methods, so
  `ConditionalWriterFor(wrapper)` fails the direct assert, the wrapper has no
  handle, and CachingStore returns unsupported: the merge-gate test dies with
  `ErrConditionalWriteUnsupported` before testing anything. **EVERY backing
  wrapper these tests use** (not just the fail-refresh-once one) MUST define the
  four CAS methods delegating to the inner store (mirror
  `countingBackingStore.ReleaseIfCurrent` :42-49) or implement
  `ConditionalWriterHandle()`. Also note `countingBackingStore` does NOT count
  `Get` today (only SetMetadata/Batch/Update/Close/ReleaseIfCurrent) — the
  anti-vacuity assertion needs `Get` counting, so extend it or write one new
  combined counting + fail-once + CAS-delegating wrapper for the merge gate.
- **G5 — the conformance row must live in an EXTERNAL test file** (`package
  beads_test`, like memstore_test.go:23 / filestore_test.go:105): `beadstest`
  imports `beads`, so a `package beads` internal test importing `beadstest` is an
  import cycle. The white-box evict assertions (`c.beads`/`c.dirty` map state)
  need `package beads`. Hence TWO test files: an internal one (merge gate +
  evict-on-precondition + bookkeeping) and an external one (conformance row +
  unsupported legs).
- **G6 — prime or the assertions are vacuous.** An unprimed cache (cold state)
  routes every `Get` straight to backing (:444-445) — "next Get hits backing"
  then proves nothing. `Prime(context.Background())` (caching_store.go:450) or
  create-through-cache first, and use a counting wrapper to prove cache-served
  reads pre-evict (the anti-vacuity requirement above).
- **G7 — refresh reads `c.backing.Get`, never `c.Get`.** Reuse
  `refreshBeadAfterWrite` (:437-444) — routing the refresh through the cache
  would serve the very entry being maintained (a mutant worth running: it makes
  the convergence test fail via a stale self-read).
- **G8 — forward errors untouched.** BdStore already stamped
  `ID`/`Expected`/`Verb`; MemStore/FileStore stamp natively. CachingStore adds
  cache maintenance, not error decoration. (`errors.As` on the forwarded error
  for the evict trigger; return the original.)

## Design-pass items (resolve in the bounded Fable pass BEFORE coding)

- **D1** Exact evict bookkeeping composition (G2's sketch): dirty-set vs plain
  delete; whether `markFreshLocked` belongs on a failure path (the existing
  refresh-failure branches do call it); whether `clearDependentReadyProjectionsLocked`
  should run on precondition-evict (state unknown → safest yes; cost = ready-projection
  recompute per contention event).
- **D2** `notifyChange` semantics on fenced paths: success+refresh → mirror the
  unconditional counterparts (`bead.updated`/`bead.closed`/`bead.deleted`);
  success+evict → there is no fresh bead to attach (the ReleaseIfCurrent
  precedent notifies from a patched clone — not available under evict-never-patch);
  recommend skip-notify on the evict leg and let reconcile catch up, but decide
  explicitly. Update's ErrNotFound branch (:74-95) synthesizes a `bead.closed`
  from the cached clone — a precedent for synthesizing if the pass prefers it.
- **D3** Cache action on CAS `(false, nil)` value-loss: none is mandated (§8.5
  mandates evict only on precondition + refresh-fail). A (false,nil) means the
  backing disagreed with whatever the caller believed — the cached value MAY be
  stale until reconcile. Recommend: no cache action (no write proven; the
  harness only requires winner-side visibility, which the winner's own refresh
  provides). Decide and document.
- **D4** `CASRetriesExhaustedError` / `GateRefusalError` / ambiguous errors from
  backing: no evict mandated. Exhaustion and gate refusal prove nothing about
  THIS entry's staleness (gate refusal = write didn't commit). Ambiguous
  (may-have-committed) is the interesting one — the cached entry may now be
  stale; marking dirty is cheap insurance. Recommend: fold dirty-on-ambiguous
  only if the design pass concurs; it is NOT in §8.5's mandate and can't be
  exercised through a MemStore backing (note it as residue if skipped).
- **D5** Method placement: the build spec says `caching_store_writes.go`, but
  that file is already ~990 lines; a new `caching_store_conditional.go` mirrors
  the Phase 4/5 `bdstore_conditional.go` convention and keeps the diff isolated.
  Recommend the new file; record the divergence in the build spec either way.

## Mutation battery candidates (prove the teeth; run after green, before red-team)

- **M1** Re-add the optimistic patch on the CAS refresh-fail leg (the §8.5
  poison) → merge gate must FAIL.
- **M2** Drop evict-on-precondition → the stale-cache retry test must FAIL.
- **M3** Evict stamps `deletedSeq` → merge gate must FAIL (Get returns
  ErrNotFound instead of hitting backing — G1's exact failure mode).
- **M4** Refresh via `c.Get` instead of `c.backing.Get` → convergence must FAIL
  (stale self-read defeats the refresh).
- **M5** `UpdateIfMatch` forwards to the UNCONDITIONAL `backing.Update` → the
  precondition leg must FAIL (no precondition ever surfaces).
- **M6** Drop the `ConditionalWriterFor(c.backing)` miss branch (return nil
  instead of typed unsupported) → the capability-absent leg must FAIL.
- **M7** Add a `metadataAlreadyMatchesCached` short-circuit to
  `CompareAndSetMetadataKey` → conformance contention/winner-visibility legs
  must FAIL (fabricated success without a backing write).

## Process (the user's standing method — non-negotiable)
1. **Bounded Fable design pass** over D1–D5 + the merge-gate test shape. Spawn a
   single subagent via the Agent tool with `model: "fable"` and a self-contained
   numbered-questions prompt (no repo exploration needed if you inline the facts);
   ask for DECISION + short justification per question. Keep the ask BOUNDED —
   one focused critique, never "write the whole design/spec": a past design agent
   stalled permanently on one oversized single-file output (memory:
   `workflow-big-generation-stall`). The Phase-4 pass caught 5 real defects,
   Phase-5's validated the two load-bearing retry choices — this step pays.
2. **TDD** red-first: merge gate + evict-on-precondition + bookkeeping asserts
   (internal file), conformance row + unsupported legs (external file) → impl
   green. ≤5 files.
3. **Mutation battery** (M1–M7): backup to `$CLAUDE_JOB_DIR/tmp`, python
   line-precise string-replace, run the specific test (expect FAIL = teeth),
   `cp -f` restore, `diff` byte-identical. **NEVER `git checkout`** (wipes the
   uncommitted stack — there should be none, but the rule stands).
4. **Fable red-team BEFORE the commit** — another Agent-tool subagent with
   `model: "fable"`, prompted READ-ONLY (it must not edit or run mutations
   itself): uncommitted changes mean an isolated worktree won't see them, and a
   past red-team agent mutated the shared tree in place and left residue (memory:
   `redteam-mutation-shared-worktree`). It PROPOSES mutations/bugs; you RUN them
   in the main loop to prove teeth. Phase 5's red-team found a real argv
   inconsistency (the CAS `--json` omission) + 4 coverage gaps; fold confirmed
   findings, document refuted ones with cause.
5. **Full gates**, then commit. Trailer:
   `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`.
   **Do NOT push.**

## Gates / verification
- `go build ./internal/beads/...` AND `go build -tags gascity_native_beads ./internal/beads/...`
- `go test ./internal/beads/ ./internal/beads/beadstest/` — FULL package, not `-run`
- `go test -tags gascity_native_beads ./internal/beads/` — full, native tag
- `go test ./internal/beads/ -run 'Conditional|CachingStoreCAS' -race` (the
  conformance contention leg runs 16 goroutines through the cache mutexes)
- `go vet ./internal/beads/...` (both tags); `golangci-lint run ./internal/beads/`
  AND `golangci-lint run --build-tags gascity_native_beads ./internal/beads/`
  (retry on "parallel golangci-lint is running"). The native-tag run has **32
  PRE-EXISTING issues** in doltlite_read_store.go (DepRemove/DepList doc comments
  etc.) that the plain run never surfaces. Zero-net-new recipe: BEFORE touching
  code, `golangci-lint run --build-tags gascity_native_beads ./internal/beads/ >
  "$CLAUDE_JOB_DIR/tmp/lint-native-baseline.txt" 2>&1 || true`; after changes,
  re-run and diff — require zero new lines (do NOT use git stash to baseline).
- `gofumpt -l <changed>` (`/home/ubuntu/go/bin/gofumpt`) → empty
- Wire gate: `go test ./internal/api/ -run 'OpenAPISpecInSync|EventPayload'`
  (Phase 6 touches no wire type — confirm anyway)
- Pre-commit hook runs clean (lint-changed + doc-gen + vet + docsync)

## Gotchas (standing, learned this stack)
- **Dolt is LOCAL-ONLY** — `git push` only; never `bd dolt push/pull/remote`.
- Stage ONLY your files; the two non-mine untracked `engdocs/plans/` dirs stay untracked.
- **`go clean -cache` is BANNED**; cold build via `GOCACHE=$(mktemp -d)`. `-testcache` is fine.
- Pre-existing `-race` flake in `internal/api`: `TestStreamSessionPeekAcceptsPeekCapability`
  (bd `ga-69hv8k`) — not rollout; filter it out if racing that package.
- `PreconditionFailedError` ≠ `CASRetriesExhaustedError` (distinct types, no
  wrapping) — `TestConditionalWriterErrorIdentity` pins it; don't blur them in
  the evict trigger (`errors.As` for precondition only).
- The revision contract carves derived-projection columns (bd `is_blocked`) OUT
  of the bump guarantee — conformance never asserts cross-bead/dep bumps.

## After Phase 6
- **PR-S2a is code-complete** → then **PR-S2b**: S2-T10 factory mode-stamp +
  `ResolveConditionalWriter` thin adapter over the general `rollout` resolver;
  S2-T11 `beads.conditional_writes.degraded` event REGISTERED-only (typed-events
  CI gate: `events.RegisterPayload` or `events.NoPayload`, wire gate must stay
  green); S2-T12 the `//go:build integration` BdStore conformance row against a
  #4682-capable bd (the authoritative guard for the provisional
  `precondition-failed`/`conditional-write-unsupported` body codes AND the three
  adversarial inputs recorded in the build spec). S2-T9 sqlite stays deferred
  out of S2.
- **Checkpoint with the user before S3** — S3 is outward-facing (deploy-lineage
  sync + the live maintainer-city flip). Do not start it unprompted.
