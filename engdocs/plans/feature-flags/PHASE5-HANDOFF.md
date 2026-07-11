# Phase 5 Handoff — BdStore ConditionalWriter verbs + CAS emulation (then the F2 Doltlite fix)

Pick-up doc for the next session. **PR-S2a Phases 1–4 are complete** (interface,
typed errors, conformance harness, MemStore + FileStore native CAS, and the
BdStore result classifier + capability probe). This hands off **Phase 5**
(S2-T6/T7: the three `*IfMatch` verbs + a dedicated retry wrapper + bounded
metadata-CAS emulation + the SQL-spike verdict), which is also where **F2 (bd
`ga-zj78gu`) fires** and MUST be fixed. Then **Phase 6** (S2-T8: CachingStore
evict-never-patch, the livelock merge gate). All still **INERT** — zero consumers.

The kickoff prompt is `PHASE5-PROMPT.md` (paste it to start). This doc is the
grounding; the build spec is `PR-S2a-BUILD-SPEC.md` (keep its Progress block current).

---

## Status — what's committed (branch `worktree-reconciler`, local/UNPUSHED)

| Commit | Task | Content |
|--------|------|---------|
| `44eb2ab70` | docs | `PR-S2a-BUILD-SPEC.md` (keep its Progress block current) |
| `bec9156b1` | S2-T1 | `ConditionalWriter` interface + normative revision/granularity contract; 4 typed errors (`ErrConditionalWriteUnsupported`, `PreconditionFailedError`, `GateRefusalError`, `CASRetriesExhaustedError`) + `IsX` helpers; `Bead.Revision int64 json:"-"`; `ConditionalWriterFor`/`ConditionalWriterHandleProvider`; `bdIssue.Revision` decode + `toBead` stamping |
| `ec0bccd04` | S2-T2, T3-mem | `beadstest/conditional_writer_conformance.go` harness; MemStore native CAS + `DisableConditionalWrites` |
| `da0d073a6` | S2-T3-file | FileStore native CAS (flock-wrapped) + out-of-band `fileData.Revisions` persistence |
| `6c0160669` | **S2-T4/T5** | **BdStore classifier + probe** — `internal/beads/bdstore_conditional.go` + `condWrite*` fields on `bdstore.go` + `bdstore_conditional_internal_test.go` (34 subtests, 13 mutations killed) |

**Do not push** (local integration stack). **Two untracked dirs are NOT mine** —
`engdocs/plans/beads-cas/`, `engdocs/plans/reconciler-redesign/`. Leave them; never `git add` them.

## The three resolved decisions (still authoritative — override any stale plan wording)
1. `Bead.Revision` is `json:"-"` (wire byte-untouched). DONE.
2. **The bd classifier is MESSAGE/BODY-substring matching, not a numeric exit-code path.** Confirmed at source in Phase 4: real bd v1.1.0-rc.1 has no `--if-revision`, exits **1** for every error, and its envelope (`{"error","hint","schema_version"}` flat or `{"schema_version","data":{...}}` wrapped) has **no `code` field yet**. The plan's "exit-9/exit-13" is a misnomer for this codebase.
3. Method names mirror `Update`/`Close`/`Delete` → `UpdateIfMatch`/`CloseIfMatch`/`DeleteIfMatch`/`CompareAndSetMetadataKey`. DONE in the interface (`beads.go:168-186`).

---

## Phase 4 API surface that Phase 5 consumes (all in `bdstore_conditional.go`)

- `classifyConditionalWriteResult(out []byte, err error) error` — **pure**, message/body-based. Returns `nil` (success) · `*PreconditionFailedError{Expected,Current,Raw}` **with `ID:""`** · `ErrConditionalWriteUnsupported` (the LATCHING veto) · `*GateRefusalError{Code,Raw}` · the ambiguous error as-is · `ErrNotFound` · else the error as-is. **Contract for the Phase-5 verb wrappers:** the classifier fills `Expected`/`Current` from bd's body when present but leaves `ID:""`; the verb wrapper MUST (a) stamp `pfe.ID = id`, and (b) **override `pfe.Expected` with the caller's own `expectedRevision` argument** — the conformance harness asserts `Expected == the stale revision the caller passed` UNCONDITIONALLY (`conditional_writer_conformance.go:219`); only `Current` is gated behind `ConditionalWriterOptions.SuppliesCurrent`. Use `errors.As(err, &pfe)` to detect and re-stamp.
- `conditionalWritesCapable() (bool, error)` — lazy, memoized four-verb (`update`/`close`/`assign`/`delete`) `--help` grep for `--if-revision` through `s.runner`. Call this FIRST in every verb; on `false` return `ErrConditionalWriteUnsupported` (do NOT fall through to an unconditional write).
- `markConditionalWritesUnsupported()` — latches the store incapable; authoritative over the probe. **Call it whenever a verb's classifier result is `ErrConditionalWriteUnsupported`** (a real runtime unsupported response must halt every future fenced write on this store, not just this one).

## Phase 5 build tasks (S2-T6 + S2-T7) — plan `EXECUTION-PLAN.md` ~lines 139-170, DESIGN §8.2 (retry) / §8.4 (emulation + spike)

All new code lands in `internal/beads/bdstore_conditional.go`; tests in
`bdstore_conditional_internal_test.go`. ≤5 files/phase.

### 1. The three `*IfMatch` verbs (S2-T6)
Each: `capable, _ := s.conditionalWritesCapable(); if !capable { return ErrConditionalWriteUnsupported }`
→ build the same argv the unconditional verb builds (mirror `Update` bdstore.go:1049, `close`/`bdCloseArgs` :2053, `delete` :2104) **plus** `--if-revision N` (`strconv.FormatInt(expectedRevision, 10)`) and `--json` →
run through the NEW `runConditionalWrite` wrapper (below) →
`err := classifyConditionalWriteResult(out, err)`; if `IsConditionalWriteUnsupported(err)` → `s.markConditionalWritesUnsupported()`; if a `*PreconditionFailedError`, stamp `ID`+override `Expected` (see contract above); return.
- `UpdateIfMatch(id, expectedRevision, opts)` — reuse the exact opts→argv fan-out from `Update` (title/status/type/priority/description/parent/assignee/metadata/labels). The empty-update no-op guard (`len(args)==3`) must stay.
- `CloseIfMatch(id, expectedRevision)` — mirror `close`; keep bd's exit-0-but-not-closed honesty re-read? NO — for CAS, a precondition/gate result must surface, not be masked by a re-read. Keep it simple: run the fenced close, classify, return. (The unconditional `close`'s import-revert re-read is a separate concern; do not port it into the fenced path without thinking through how it interacts with a precondition failure.)
- `DeleteIfMatch(id, expectedRevision)` — mirror `delete --force`.

### 2. `runConditionalWrite` — the dedicated retry wrapper (S2-T6, DESIGN §8.2 last ¶)
**MUST NOT route through `runBDTransientWrite`/`runBDTransientWriteOutputWhen`/`isBdTransientWriteError`** (bdstore.go:1794-1824). Replaying a stale `--if-revision N` after a connection error is wrong (the first attempt may have committed and bumped the revision); blind retry of a precondition failure converts a signal into a spin. Retry policy:
- connection/serialization-class error (`isBdTransientWriteError`) → **re-read the bead's revision before any re-attempt** (bounded, jittered). The whole point: never replay a stale fence.
- precondition failure → **surface to the caller immediately** (the caller re-reads and re-decides — that IS CAS).
- ambiguous (`isBdAmbiguousWriteError`) → surface **as-is** (the write may have committed).
- **never downgrade to an unconditional write.**
- **The doltlite `--dolt-auto-commit off` prefix must still apply** — the blind loop gets it from `s.bdTransientWriteArgs(args)` (bdstore.go:1845, gated on `s.isDoltliteBackend()`); your wrapper must call `s.bdTransientWriteArgs(args)` too or the doltlite backend writes without it.

### 3. `CompareAndSetMetadataKey` — bounded emulation + typed exhaustion (S2-T7, DESIGN §8.4 verbatim)
```
const casEmulationMaxAttempts = 4
const casEmulationBaseBackoff = 25 * time.Millisecond // doubles per attempt, jittered
```
Loop: `Get(id)` → if `b.Metadata[key] != expected` return `(false, nil)` (`""≡absent`, genuine value loss) → `runConditionalWrite(id, b.Revision, "update", id, "--set-metadata", key+"="+next, "--if-revision", ...)` → `nil`→`(true,nil)`; `errors.As(&pre)`→ on the last attempt return `*CASRetriesExhaustedError{ID,Key,Attempts,LastRevision:b.Revision}` (NOT `PreconditionFailedError`, NOT `(false,nil)` — exhaustion is a transient, the value never mismatched), else sleep-with-jitter and retry; any other error → `(false, err)` as-is.
- **Testability:** there is NO jitter/backoff helper in `internal/beads` today. Add one with a **seam** so tests are deterministic and fast — e.g. a package-level `casEmulationSleep = func(time.Duration){...}` (or an unexported field) the test overrides to a no-op, so the contention/exhaustion subtests don't actually sleep 25→200ms×racers. `math/rand` is fine in production code (banned only in Workflow scripts).

### 4. The SQL-spike verdict (S2-T7, DESIGN §8.4 "Sidestep under evaluation")
Evaluate replacing the emulation loop with a single conditional SQL `UPDATE` (the `ReleaseIfCurrent` template at bdstore.go:1107 + its `releaseIfCurrentViaEmbeddedDoltSQL` fallback :1128, with a JSON-path predicate on the metadata column). **Disqualifier the spike MUST clear:** the raw `UPDATE` bypasses bd's write layer, so it must ALSO `revision = revision + 1` atomically or it breaks the revision contract for every OTHER conditional writer. bd #4682 (which adds the revision column) is unlanded, so the column can't be bumped today → **recommended verdict: emulation loop SHIPS; SQL path dropped, not half-adopted.** Record a **dated note** in `engdocs/plans/feature-flags/` (a short `S2-T7-sql-spike-verdict.md` or a dated entry in the build spec).

### 5. Compile assert
Add `var _ ConditionalWriter = (*BdStore)(nil)` **here** (it compiles only once all four methods exist).

### 6. Conformance over BdStore (S2-T7)
Run `beadstest.RunConditionalWriterConformanceWithOptions` over BdStore backed by a **scripted fake runner** where scriptable (per DESIGN §7.4 you need a runner whose apply-func mutates fake backing state and can return the committed-but-ambiguous cell). Unit conformance for BdStore is inherently limited (no real bd); the **authoritative row is the `//go:build integration` build against a #4682-capable bd (S2-T12, PR-S2b)** — say so. `SuppliesCurrent` should be set only if the scripted bd body reliably carries `current_revision`.

---

## F2 — bd `ga-zj78gu` — MUST-FIX in Phase 5 (read the bead first: `bd show ga-zj78gu`)

The moment BdStore implements `ConditionalWriter`, `DoltliteReadStore`
(`internal/beads/doltlite_read_store.go:22`, embeds a concrete `*BdStore`)
**falsely promotes** the capability:

- **Why promotion can't be suppressed by a handle alone.** `ConditionalWriterFor` (beads.go:202) does the **direct** `store.(ConditionalWriter)` assertion FIRST and only consults `ConditionalWriterHandleProvider` if that fails. The embedded `*BdStore`'s methods promote, so the direct assertion on `*DoltliteReadStore` succeeds → the provider is never reached. Adding `ConditionalWriterHandle() (nil,false)` does NOT help. To change the answer you must **define the four CAS methods on `*DoltliteReadStore`** (own methods shadow promoted ones).
- **Why the promoted CAS is broken.** `DoltliteReadStore.Get` (:174) reads via direct SQL (`queryIssues`), which does not populate `Bead.Revision` (no revision column pre-#4682) → returns 0. A consumer reads the revision through `DoltliteReadStore.Get` (0) and passes it to a CAS verb, but the verb re-reads/fences via the embedded `BdStore.Get` (bd subprocess, the real revision N post-#4682) → `PreconditionFailedError` forever in the `GC_NATIVE_DOLTLITE_BEADS` deployment. Promoted writes also bypass every mutator's `resetOrderRunCache()` (the wrapper overrides Create/Update/Close/CloseAll/Reopen/Delete/SetMetadata*/DepAdd at :506-605 to invalidate its caches; the promoted CAS verbs would skip that).
- **Recommended Phase-5 fix (pre-#4682, inert-safe):** override the four CAS methods on `*DoltliteReadStore` to **loudly degrade** — return `ErrConditionalWriteUnsupported` (and `(false, ErrConditionalWriteUnsupported)` for `CompareAndSetMetadataKey`), documented: *"the direct-SQL read path cannot supply CAS revisions until bd #4682 adds the revision column; degrade rather than false-promote a store whose reads and fenced writes disagree on the revision source."* This is the sanctioned typed-unsupported pattern (same shape as MemStore's `DisableConditionalWrites`), NOT a hiding wrapper. Test: `ConditionalWriterFor(doltliteStore)` still asserts true, but all four verbs return unsupported; the order-run cache is untouched (nothing mutates).
- **Post-#4682 upgrade path (note it, don't build it):** once bd #4682 lands AND the doltlite SQL schema carries the revision column, switch to populating `Revision` in `DoltliteReadStore.Get`/scanBead AND override the four verbs to do the real fenced write + `resetOrderRunCache()`.
- **Secondary audit:** `internal/beads/exec/exec.go:163` (`beadWire.toBead`) is a SECOND bd-JSON envelope that drops revision — lower risk (exec stores won't claim the capability), but confirm no exec-backed store reaches CAS before shipping.

**Do this in the SAME phase as the verbs** (the compile-assert `var _ ConditionalWriter = (*BdStore)(nil)` is what activates the promotion; ship the fix alongside it). Close `ga-zj78gu` when done.

---

## BdStore surface map for Phase 5 (verified file:line — don't re-explore)
- **Verbs to mirror:** `Update` bdstore.go:1049 (full opts→argv fan-out + `len(args)==3` no-op guard); `bdCloseArgs` :2053 + `close` :2061; `Reopen` :2089; `Delete` :2101; `Get` :1016 (returns `bead`, maps `isBdNotFound`→ErrNotFound + the ID-collision guard).
- **Blind loop to AVOID:** `runBDTransientWrite`/`runBDTransientWriteOutput`/`runBDTransientWriteOutputWhen` :1794-1824 (budget `bdTransientWriteAttempts=3`, 25ms×attempt). `isBdTransientWriteError` :1883, `isBdAmbiguousWriteError` :1894.
- **doltlite prefix:** `s.bdTransientWriteArgs(args)` :1845 (prepends `--dolt-auto-commit off` when `s.isDoltliteBackend()` :1854). Your `runConditionalWrite` must apply it.
- **SQL-spike precedent:** `ReleaseIfCurrent` :1107 (`bd sql --json UPDATE … WHERE …`, reads `rows_affected`), embedded-dolt fallback `releaseIfCurrentViaEmbeddedDoltSQL` :1128, string escaping `bdSQLStringLiteral`, `extractJSON` :485.
- **Metadata write:** the emulation uses `update --set-metadata key=val` argv (there is no dedicated single-key bd write worth reusing for the fenced path).
- **Test double:** `fakeRunner` (bdstore_test.go:19, `package beads_test`) is keyed `name+" "+join(args)` → `{out,err}`; it works for distinct argv but has **no per-call apply-func**. Phase 5 needs a richer **scripted** runner (white-box, `package beads`) whose apply-func mutates fake backing state BEFORE returning (for the committed-but-ambiguous cell and the re-read-on-transient path). Put it in `bdstore_conditional_internal_test.go`.

## Process (the user's standing method — non-negotiable)
1. **Fable design pass** (model `fable`, model override on the Agent tool) over the phase — resolve the `runConditionalWrite` retry shape, the scripted-runner design, the emulation seam, and the SQL-spike verdict. **Keep the ask BOUNDED** (a design agent stalled once on an oversized single output — `[[workflow-big-generation-stall]]`); prefer a focused critique or synthesize in the main loop. Phase 4's design-pass caught 5 real defects — this pays off.
2. **TDD** in the main loop: red-first tests (verb success/precondition/unsupported-latch legs via the scripted runner; the emulation loop's win/lose/exhaustion/contention; the F2 degrade) → impl green. ≤5 files; verify each.
3. **Fable red-team BEFORE the commit** — read-only on the shared worktree (uncommitted changes mean an isolated worktree won't see them; `[[redteam-mutation-shared-worktree]]`). Have it PROPOSE mutations; **run them yourself in the main loop to prove teeth** (Phase 4's red-team empirically ran mutations and found 4 surviving mutants + 2 real parse defects — the design critique alone missed them). Fold confirmed findings; document residue.
4. **Full gates**, then commit. Trailer: `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`. **Do NOT push.**

## Gates / verification (every phase)
- `go build ./internal/beads/...`
- `go test ./internal/beads/ ./internal/beads/beadstest/` — **FULL package, not `-run`** (the pre-commit hook skips package guards; latent failures only show in a full run).
- `go test ./internal/beads/ -run 'Conditional' -race` (the emulation retry loop is the first goroutine-adjacent CAS path — keep `-race`).
- `go vet ./internal/beads/...` ; `golangci-lint run ./internal/beads/` (retry on "parallel golangci-lint is running"; rename unused closure params to `_`).
- `gofumpt -l <changed>` (binary `/home/ubuntu/go/bin/gofumpt`) → empty.
- **Wire gate:** `go test ./internal/api/ -run 'OpenAPISpecInSync|EventPayload'` (Phase 5 touches no wire type, but confirm — F2 edits `beads.Bead` revision population, not the wire tag).
- Pre-commit hook (lint-changed + doc-gen + vet + docsync) runs clean on this worktree.

## Mutation-testing discipline (proves test teeth — the load-bearing habit)
Back up the changed file to `$CLAUDE_JOB_DIR/tmp` (`/home/ubuntu/.claude/jobs/<id>/tmp`), mutate with a `python3` string-replace, run the specific subtest (expect FAIL = teeth), then `cp -f` the backup back and `diff` to confirm byte-identical. **NEVER `git checkout <file>`** to revert — it wipes ALL uncommitted changes in the stack. Phase 4 killed 13 mutations this way.

## Gotchas (learned this stack)
- **Dolt is LOCAL-ONLY** — `git push` only; never `bd dolt push/pull/remote`.
- Stage ONLY your files at commit (`git add <files>`); the two non-mine untracked `engdocs/plans/` dirs must stay untracked.
- **`go clean -cache` is BANNED**; cold build via `GOCACHE=$(mktemp -d) go build ...`. `go clean -testcache` is allowed.
- **Pre-existing flake:** `TestStreamSessionPeekAcceptsPeekCapability` reds under `-race` in `internal/api` (bd `ga-69hv8k`) — not rollout; filter it out.
- The revision contract carves derived-projection columns (bd `is_blocked`) OUT of the bump guarantee (F1) — conformance must never assert whether a cross-bead/dep write bumps.
- `PreconditionFailedError` and `CASRetriesExhaustedError` are distinct types with NO wrapping between them (an exhaustion must not `errors.As` to a precondition) — the identity test `TestConditionalWriterErrorIdentity` pins this; don't blur them.

## Reusable assets from Phases 1–4 (use them; don't reinvent)
- Harness: `beadstest.RunConditionalWriterConformanceWithOptions(t, name, open, ConditionalWriterOptions{SuppliesCurrent, OpenDisabled})`.
- Typed errors + `IsPreconditionFailed`/`IsGateRefusal`/`IsCASRetriesExhausted`/`IsConditionalWriteUnsupported` (beads.go:235-294).
- The Phase-4 classifier/probe/latch (see the API surface section above) — the verbs are thin wrappers over `runConditionalWrite` + `classifyConditionalWriteResult`.

## After Phase 5
- **Phase 6 = S2-T8** (`caching_store_writes.go` or a new `caching_store_conditional.go`): CachingStore forwards CAS to `c.backing` via `ConditionalWriterFor`; cache rule **evict, never patch** (DESIGN §8.5) — CAS success + refresh-fail → `delete(c.beads,id)` + `delete(c.deps,id)` + `dirty[id]` set + markFreshLocked/clearDependentReadyProjectionsLocked bookkeeping (**never a `deletedSeq` stamp on evict** — Get short-circuits a deletedSeq id to ErrNotFound without consulting backing; DeleteIfMatch SUCCESS is the only deletedSeq case. Corrected 2026-07-11 — an earlier revision of this line wrongly listed deletedSeq in the evict set); **every** `PreconditionFailedError` from backing → evict. **MERGE GATE test** `TestCachingStoreCASRetryLoopConverges` (refresh forced to fail once → entry evicted → next Get hits backing → retry converges, not livelock) + evict-on-precondition. Compile assert `var _ ConditionalWriter = (*CachingStore)(nil)`. **The authoritative Phase-6 pickup is now `PHASE6-HANDOFF.md` + `PHASE6-PROMPT.md`** — this bullet is superseded by them.
- Then **PR-S2b** (S2-T10 factory mode-stamp + `ResolveConditionalWriter` thin adapter over the general `rollout` resolver; S2-T11 `beads.conditional_writes.degraded` event REGISTERED-only; S2-T12 the `//go:build integration` BdStore conformance row against a #4682-capable bd). S2-T9 sqlite is deferred out of S2.
- **Checkpoint with the user before S3** — S3 is outward-facing (deploy-lineage sync + the live maintainer-city flip).
