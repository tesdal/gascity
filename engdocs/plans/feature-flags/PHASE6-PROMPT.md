# Phase 6 Kickoff Prompt

Paste the block below to start the next session.

---

Continue the gascity feature-flag rollout (PR-S2a) on branch `worktree-reconciler`
at `/data/projects/gascity/.claude/worktrees/reconciler`. Phases 1–5 are complete,
committed, local/UNPUSHED, inert: `bec9156b1` (S2-T1 interface + typed errors),
`ec0bccd04` (S2-T2 harness + S2-T3 MemStore CAS), `da0d073a6` (S2-T3 FileStore CAS),
`6c0160669` (S2-T4/T5 BdStore classifier + probe), `3f113a52a` (S2-T6/T7 BdStore
`*IfMatch` verbs + `runConditionalWrite` + CAS emulation + the F2 Doltlite
loud-degrade, bd `ga-zj78gu` CLOSED). Read
`engdocs/plans/feature-flags/PHASE6-HANDOFF.md` first — it has the full status, the
verified CachingStore surface map (Get/evict semantics, write-path bookkeeping,
test-fake precedents), the deep gotchas G1–G8, the design-pass items D1–D5, and the
mutation battery M1–M7. Build spec: `engdocs/plans/feature-flags/PR-S2a-BUILD-SPEC.md`
(keep its Progress block current). DESIGN detail:
`engdocs/plans/feature-flags/DESIGN.md` §8.5 (forward-and-evict). All bare `*.go`
paths below are `internal/beads/`.

Now build **Phase 6 = S2-T8**: CachingStore implements `ConditionalWriter` by
forwarding to `ConditionalWriterFor(c.backing)` — cache rule **EVICT, never patch**:

1. **The four forwarded methods** (`UpdateIfMatch`/`CloseIfMatch`/`DeleteIfMatch`/
   `CompareAndSetMetadataKey`): resolve the backing writer per call; miss →
   `ErrConditionalWriteUnsupported` (`(false, …)` for CAS), mirroring the
   `ReleaseIfCurrent` template (caching_store_writes.go:138-141). Forward errors
   UNTOUCHED (BdStore/MemStore already stamp ID/Expected). No idempotence
   short-circuits on any fenced path (only the backing evaluates the fence).
2. **Cache maintenance:** success + refresh ok → normal refresh bookkeeping
   (reuse `refreshBeadAfterWrite`, which reads `c.backing.Get` — NEVER `c.Get`);
   success + refresh FAILED → **EVICT** (delete from `c.beads`/`c.deps`, set
   `c.dirty[id]` — NEVER stamp `deletedSeq`, which short-circuits Get to
   ErrNotFound without consulting backing); **every `PreconditionFailedError`
   from backing → evict**; `DeleteIfMatch` success → mirror `Delete`'s full
   scrub (`deletedSeq` stamp is correct there and only there).
3. **MERGE GATE test** `TestCachingStoreCASRetryLoopConverges` (internal,
   `package beads`): CAS success + refresh forced to fail once → entry evicted →
   next Get hits backing (prove pre-evict reads were cache-served via a counting
   wrapper — anti-vacuity) → retry with the fresh revision converges. Second leg:
   out-of-band backing mutation → fenced write with the cached-stale revision →
   precondition surfaces AND evicts → re-read → retry succeeds. GOTCHA: the
   backing test-wrapper must DEFINE the four CAS methods delegating inward
   (interface embedding does not promote them — `ConditionalWriterFor` would
   report unsupported and the test dies before testing anything).
4. **Conformance row** (external, `package beads_test` — beadstest→beads import
   cycle forbids internal): `RunConditionalWriterConformanceWithOptions` over
   `NewCachingStoreForTest(NewMemStore(), nil)` + `Prime(ctx)`;
   `SuppliesCurrent: true`; `OpenDisabled` = CachingStore over a
   `DisableConditionalWrites` MemStore. Plus the capability-absent leg
   (interface-embedding wrapper strips CAS → typed unsupported).
5. Compile assert `var _ ConditionalWriter = (*CachingStore)(nil)`.

Process (non-negotiable): **bounded Fable design pass** (model `fable`) over the
handoff's D1–D5 (evict bookkeeping composition, notify semantics, (false,nil)/
ambiguous cache actions, file placement — recommend a new
`caching_store_conditional.go` over the spec's caching_store_writes.go; record the
divergence) → **TDD** red-first → **mutation battery M1–M7** (backup to
`$CLAUDE_JOB_DIR/tmp`, python string-replace, run subtest expect FAIL, `cp -f`
restore — **NEVER `git checkout`**) → **Fable red-team BEFORE the commit**
(read-only on the shared worktree; it PROPOSES, you RUN) → full gates (full
`go test ./internal/beads/ ./internal/beads/beadstest/` on BOTH build tags, `-race`
on the Conditional tests, vet, golangci-lint on both tags — the native run is
`golangci-lint run --build-tags gascity_native_beads ./internal/beads/` and has 32
PRE-EXISTING doltlite issues; baseline it to a file BEFORE coding and require zero
net-new — gofumpt, the `OpenAPISpecInSync|EventPayload` wire gate) → commit with
trailer
`Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`.

Phase 6 completes the PR-S2a code. Then **PR-S2b** (S2-T10 `ResolveConditionalWriter`
rollout adapter, S2-T11 degraded event REGISTERED-only, S2-T12 the
`//go:build integration` conformance row vs a #4682-capable bd). **Do NOT push.
Do NOT start S3** without checking in — S3 is outward-facing (deploy-lineage sync +
the live maintainer-city flip).
