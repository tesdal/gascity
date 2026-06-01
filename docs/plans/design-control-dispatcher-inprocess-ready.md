# Design: In-process control-dispatcher ready-poll (eliminate per-cycle `bd` execs)

- Status: REVISED (design review incorporated) — pending plan
- Author: agent (hermes rig)
- Tracking: techcloud0/hermes#440, bead hermes-34dw6h
- Repo: gastownhall/gascity (branch `build-native-v4`)
- Design reviews: opus + codex (both NEEDS-CHANGES on v1 → addressed below)

## Problem

After the agent VM migrated to the in-process `NativeDoltStore` (2026-06-01),
the Dolt connection/OOM storm is gone. The **residual** per-cycle process churn
comes from the control-dispatcher ready-poll loop (`gc convoy control --serve
--follow`).

`workflowServeControlReadyQuery` (`cmd/gc/dispatch_runtime.go:691`) returns a
**shell string** that the serve loop runs via `workflowServeList` →
`nextWorkflowServeBeads` → `shellWorkQueryWithEnv`. That script execs the `bd`
binary **up to ~14×/cycle**:

- `bd ready --include-ephemeral --assignee=$cand --exclude-type=epic --json
  --limit=N` per candidate id in `[GC_CONTROL_SESSION_NAME, GC_SESSION_NAME,
  GC_ALIAS, GC_CONTROL_TARGET, GC_SESSION_ID]` and each id's legacy
  `…/workflow-control` variant.
- `routed_ready` for `GC_CONTROL_TARGET` and `GC_CONTROL_LEGACY_TARGET`: two
  calls each — `--metadata-field gc.run_target=$route` and
  `--metadata-field gc.routed_to=$route`, both `--unassigned --sort oldest`.

Each is a separate OS process (28–54 MB) connecting to dolt `:13035`; one also
triggers a `dolt remote -v` shell-out. Bounded/non-OOM, but wasteful.

`gc bd` is **not** a fix: `doBd` (`cmd/gc/cmd_bd.go:180`) execs the `bd` binary.

## Goals

1. When the control-dispatcher runs against the in-process native store, perform
   the ready fan-out **in-process**, eliminating per-cycle `bd` execs and the
   `dolt remote -v` shell-out.
2. **Exact parity** with the shell path's selection semantics (candidate set,
   legacy expansion, routed metadata queries, infra+epic exclusion, ephemeral
   inclusion, open+unblocked+actionable only, per-subquery scan limit,
   first-occurrence-wins dedup ordering, **per-subquery soft-fail**).
3. Automatic, safe fallback to the **unchanged** shell query for every store
   that does not provide the in-process capability.

## Non-goals

- Changing candidate-id derivation or the shell query (kept verbatim for fallback).
- Touching `Store.Ready` / `ReadyQuery` or any non-native store implementation.

## Approach: targeted capability interface (not a global `ReadyQuery` change)

Design review converged on **blast radius** as the primary risk. `Store.Ready`
is implemented by Native, Bd, Mem, File, Exec, and Caching stores, and adding
`map`/`slice` fields to `ReadyQuery` would also break the struct-equality check
at `caching_store_reads.go:331` (`!= (ReadyQuery{})`). We therefore introduce a
**narrow capability** that only `NativeDoltStore` implements; everything else
falls through to today's exact shell behavior.

### New type + interface (`internal/beads`)

```go
// ControlReadyFilter describes one ready sub-query for the control-dispatcher
// in-process fast path. It is intentionally separate from ReadyQuery to avoid
// changing the cross-store Ready contract.
type ControlReadyFilter struct {
    Assignee     string            // exact assignee match; empty unless Unassigned
    Unassigned   bool              // match beads with Assignee == "" (zero value)
    Metadata     map[string]string // AND-match on bead metadata key=value
    ExcludeTypes []string          // ADDITIONAL exclusions on top of the store's
                                   // built-in readyExcludeTypes (e.g. "epic")
    IncludeEphemeral bool          // union the wisps tier (tier-aware actionable filter)
    Sort         SortOrder         // SortCreatedAsc == bd "--sort oldest"
    Limit        int
}

// ControlReadyQuerier is implemented only by stores that can answer a
// ControlReadyFilter in-process. Callers MUST type-assert and fall back when
// it is not implemented.
type ControlReadyQuerier interface {
    ControlReady(ControlReadyFilter) ([]Bead, error)
}
```

`NativeDoltStore.ControlReady` reuses the store's existing actionable/unblocked
filtering but makes the ephemeral check **tier-aware** (see Finding 1) and
applies the extra filters. No other store implements the interface.

### Fan-out helper (`cmd/gc`)

```go
func controlDispatcherReadyBeads(q beads.ControlReadyQuerier, candidates, routes []string, limit int) ([]hookBead, error)
```

Mirrors the shell script **exactly**:
- For each candidate (in the same order, **no pre-dedup**) and its legacy
  variant: `ControlReady{Assignee, Limit, IncludeEphemeral:true, ExcludeTypes:["epic"]}`.
- For each route: two calls with `Metadata{"gc.run_target":route}` /
  `{"gc.routed_to":route}`, `Unassigned:true`, `Sort:SortCreatedAsc`,
  `ExcludeTypes:["epic"]`, `IncludeEphemeral:true`.
- **Per-subquery soft-fail**: a sub-query error is logged/traced and skipped
  (matches shell `|| true`); only return an error if every sub-query fails.
- Apply `Limit` **per sub-query before merge**; then concatenate in order and
  dedup by id keeping **first occurrence**.
- Map `beads.Bead` → `hookBead` (same shape the shell path unmarshals).

### Serve-loop wiring (`cmd/gc/dispatch_runtime.go`)

Branch at the **consumption seam** (`workflowServeList`, ~`:437`), not just the
query-string construction sites (`:327`/`:679`): for the control-dispatcher
agent, if an in-process store handle for the scope implements
`beads.ControlReadyQuerier`, call `controlDispatcherReadyBeads` and skip the
shell entirely; otherwise run the existing `workflowServeControlReadyQuery`
string. `workflowServeControlReadyQuery` and all its tests are untouched.

> Plan must confirm a store handle is reachable at this seam; if not, obtain one
> via the same `openStoreAtForCity`/eligibility path `gc status` uses to report
> `beads_store`.

## Review findings — resolutions

1. **(MAJOR, both) Ephemeral hard-filter** `IsReadyCandidate` (`beads.go:154`)
   drops ephemeral beads unconditionally. `ControlReady` must NOT route ephemeral
   selection through the plain helper. Resolution: native path uses a tier-aware
   actionable check (`TierBoth` when `IncludeEphemeral`) so wisps are included;
   contract test asserts a `--include-ephemeral` query returns ephemeral beads.
2. **(MAJOR, both) Blast radius** Resolved by the capability interface — only
   `NativeDoltStore` changes; `ReadyQuery` and other stores untouched.
3. **(MAJOR, codex) Struct-equality compile break** Avoided entirely — no
   `map`/`slice` added to `ReadyQuery`; `caching_store_reads.go:331` unchanged.
4. **(MAJOR, codex) Error-handling parity** Resolved by explicit per-sub-query
   soft-fail in `controlDispatcherReadyBeads` (above).
5. **(MAJOR, codex) Ordering/tie-break/limit parity** Written invariants: no
   candidate pre-dedup; per-sub-query limit before merge; `SortCreatedAsc` ==
   `bd --sort oldest` (tie-break asserted in tests); first-occurrence dedup.
6. **(MINOR, codex) Wiring seam** Branch at `workflowServeList` consumption,
   per above.
7. **(MINOR, both) ExcludeTypes additive; Unassigned == "" ; in_progress**
   `ExcludeTypes` is additive to built-in `readyExcludeTypes`; `Unassigned`
   matches `Assignee == ""`; `open`-only is inherited from the actionable check
   (in-progress excluded). All stated in the type doc + asserted in tests.

## Edge cases / risks

- **Parity drift** between in-process and shell fallback is the central risk →
  golden parity tests (below) are mandatory, not optional.
- **Caching**: the native fast path bypasses `CachingStore`; acceptable since the
  backing native store is in-process and fast (no behavioral correctness impact).
- **Store handle availability** at the serve seam — plan-time verification.

## Testing strategy (TDD)

1. `internal/beads`: RED contract tests for `NativeDoltStore.ControlReady` — one
   per filter dimension (assignee, unassigned==\"\", metadata AND-match,
   exclude-type=epic additive, include-ephemeral returns wisps, sort oldest
   tie-break, limit). Then implement.
2. `cmd/gc`: RED tests for `controlDispatcherReadyBeads` with a fake
   `ControlReadyQuerier` — candidate+route fan-out, no pre-dedup, per-subquery
   limit, soft-fail-continues-on-error, first-occurrence-wins ordering
   (port the existing `PreservesQueryPriorityWhenMerging` expectations), empty
   ⇒ `nil,nil`. Then implement.
3. `cmd/gc`: RED test that the serve seam picks the in-process path when the
   store implements the capability and the shell path otherwise. Then implement.
4. **Golden parity test**: same fixture data → assert the in-process helper's id
   list equals the shell query's id list/order. Keeps the two paths honest.
5. All existing `workflowServeControlReadyQuery*` tests remain green.

## Local verification plan

- `go test ./internal/beads/... ./cmd/gc/...` on the Mac.
- `go build -o dist/gc-darwin ./cmd/gc`; run the local Mac supervisor and
  confirm control-dispatcher cycles spawn **0** `bd` procs and **0**
  `dolt remote -v`; ready selection unchanged.
- Cross-build linux/amd64; validate the same on the agent VM.

## Rollout

- Land in `build-native-v4`; ship with the native-store binary. Fallback
  guarantees safety if eligibility regresses. Durable cloud-init enablement
  tracked separately in hermes-infra.
