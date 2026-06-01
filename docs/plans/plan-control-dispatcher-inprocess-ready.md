# In-process Control-Dispatcher Ready-Poll Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Eliminate the control-dispatcher serve loop's per-cycle `bd` execs (~8-14/cycle) and the `dolt remote -v` shell-out by answering the ready fan-out in-process when the active store is `NativeDoltStore`, with exact parity to the existing shell query and automatic fallback for all other stores.

**Architecture:** Add a narrow capability interface `beads.ControlReadyQuerier` implemented ONLY by `NativeDoltStore` (thin map of a new `ControlReadyFilter` → `beadslib.WorkFilter`, reusing `GetReadyWork`'s unblocked/actionable filtering). A `cmd/gc` fan-out helper mirrors the shell script's candidate/route expansion and first-occurrence dedup. The serve seam (`drainWorkflowServeWork`) lazily opens a store via the same `openStoreAtForCity` path `gc status` uses, type-asserts the capability, and branches; if the assertion fails it runs today's unchanged `workflowServeControlReadyQuery` shell string.

**Tech Stack:** Go 1.26, `github.com/steveyegge/beads` (`beadslib`), Cobra CLI, gascity `cmd/gc` + `internal/beads`.

**Design doc:** `docs/plans/design-control-dispatcher-inprocess-ready.md` (REVISED v2).

**Status:** REVISED v2 — round-2 plan review (opus + codex) incorporated; ready for TDD. See "Round-2 plan-review resolutions" at the end.

**Branch:** `build-native-v4`. **Tracking:** techcloud0/hermes#440, bead hermes-34dw6h.

---

## Key facts established during plan research (cite these; do not re-investigate)

- `beadslib.WorkFilter` (`steveyegge/beads@v1.0.5/internal/types/types.go:1320`) has fields:
  `Status`, `Assignee *string`, `Unassigned bool`, `IncludeEphemeral bool`,
  `ExcludeTypes []IssueType` (additive to built-in defaults; **ignored when `Type` set**),
  `MetadataFields map[string]string` (AND equality), `Limit int`, `SortPolicy SortPolicy`.
- `SortPolicyOldest SortPolicy = "oldest"` (types.go:1307) == `bd --sort oldest` == our `SortCreatedAsc`.
- `storage.GetReadyWork(ctx, WorkFilter)` already performs open-status + actionable + **dependency/unblocked** + ephemeral + exclude-type + metadata filtering server-side. `NativeDoltStore.Ready` (`native_dolt_store.go:424`) loops `nativeDoltOpenReadyStatuses` (`:19`), calls `GetReadyWork`, maps via `beadFromNativeIssue`, post-filters with `IsReadyCandidate`, dedups by id, applies limit.
- `IsReadyCandidate` (`internal/beads/beads.go:154`) hard-drops `b.Ephemeral` — **must not be used as the post-filter when `IncludeEphemeral` is set** (Finding 1).
- `readyExcludeTypes` (`beads.go:133`) is the built-in exclusion set; `epic` is NOT in it, so `ExcludeTypes:["epic"]` is genuinely additive.
- Store interface: `internal/beads/beads.go:207`. Concrete native store + compile assertion: `internal/beads/native_dolt_store.go:115`/`:122`.
- `SortOrder` (`SortDefault`/`SortCreatedAsc`/`SortCreatedDesc`) and `TierMode` live in `internal/beads/query.go:13`/`:28`.
- Serve seam: `drainWorkflowServeWork(agentCfg config.Agent, cityPath, storePath, workQuery string, workEnv map[string]string, stderr io.Writer)` at `cmd/gc/dispatch_runtime.go:433`; the seam call is `queue, err := workflowServeList(serveQuery, storePath, workEnv)` at `:438`. `workflowServeList` is a swappable `var` (`:69`) = `nextWorkflowServeBeads` (`:742`); ~90 test reassignments depend on its pure-string signature — **do not change it**.
- Store handle obtainable WITHOUT new plumbing: `openStoreAtForCity(storePath, cityPath string) (beads.Store, error)` (`cmd/gc/main.go:1165`). Both args are in scope in `drainWorkflowServeWork`.
- Control-dispatcher gate: `isWorkflowServeControlDispatcherAgent(agentCfg) bool` (`cmd/gc/dispatch_runtime.go:685`).
- Shell query (verbatim, kept for fallback): `workflowServeControlReadyQuery` (`:691`). Candidate id loop order: `$GC_CONTROL_SESSION_NAME $GC_SESSION_NAME $GC_ALIAS $GC_CONTROL_TARGET $GC_SESSION_ID`; per id, legacy variant when id ends with `control-dispatcher` → `${id%control-dispatcher}workflow-control`; per-candidate ready is `--assignee=$cand --include-ephemeral --exclude-type=epic --limit=N`. Routes: `routed_ready $GC_CONTROL_TARGET` then `routed_ready ${GC_CONTROL_LEGACY_TARGET}`, each emitting two queries (`gc.run_target=$route` and `gc.routed_to=$route`, `--unassigned --include-ephemeral --exclude-type=epic --sort oldest --limit=N`). Merge: jq reduce, first-occurrence-wins.
- `GC_CONTROL_TARGET` = `agentCfg.QualifiedName()` (fallback `config.ControlDispatcherAgentName`); `GC_CONTROL_LEGACY_TARGET` = `workflowServeLegacyControlRoute(target)` (`:730`). `GC_CONTROL_SESSION_NAME`, `GC_SESSION_NAME`, `GC_ALIAS`, `GC_SESSION_ID` come from the resolved env (`workEnv` merged over `os.Environ()`).
- `hookBead` shape: `{ID string json:"id"; Metadata hookBeadMetadata json:"metadata"}` (`dispatch_runtime.go:140`).
- `workflowServeScanLimit` is the per-subquery scan limit constant used by the shell builder (`:696`).

---

## File structure

- `internal/beads/query.go` — add `ControlReadyFilter` struct + `ControlReadyQuerier` interface (co-located with `ReadyQuery`/`ListQuery`/`SortOrder`/`TierMode`).
- `internal/beads/native_dolt_store.go` — add `func (s *NativeDoltStore) ControlReady(ControlReadyFilter) ([]Bead, error)` + compile assertion `var _ ControlReadyQuerier = (*NativeDoltStore)(nil)`.
- `internal/beads/native_dolt_store_control_ready_test.go` — NEW, contract tests for `ControlReady`.
- `cmd/gc/dispatch_runtime.go` — add `deriveControlReadyTargets`, `controlDispatcherReadyBeads`, and wiring in `drainWorkflowServeWork`. Add a swappable `var controlReadyStoreOpener` for test injection.
- `cmd/gc/dispatch_control_ready_test.go` — NEW, tests for derivation, fan-out, seam selection, and golden parity.

---

## Task 1: Capability type + interface (`internal/beads`)

**Files:**
- Modify: `internal/beads/query.go` (append after the `ReadyQuery` block)
- Modify: `internal/beads/native_dolt_store.go:122` (add compile assertion)
- Test: `internal/beads/native_dolt_store_control_ready_test.go` (create)

- [ ] **Step 1: Write the failing compile-assertion test**

Create `internal/beads/native_dolt_store_control_ready_test.go`:

```go
package beads

// Compile-time assertion that NativeDoltStore satisfies the new capability.
// This file fails to compile until Task 1 + Task 2 land.
var _ ControlReadyQuerier = (*NativeDoltStore)(nil)
```

- [ ] **Step 2: Run to verify it fails (does not compile under test)**

Run: `go test ./internal/beads/ -run TestControlReady`
Expected: FAIL — build error `undefined: ControlReadyQuerier`.

> Review note (MAJOR, RED step): use `go test`, NOT `go build` — `go build` does
> not compile `_test.go` files, so the assertion would be silently skipped and the
> step would falsely pass.

- [ ] **Step 3: Add the type + interface**

Append to `internal/beads/query.go`:

```go
// ControlReadyFilter describes one ready sub-query for the control-dispatcher
// in-process fast path. It is intentionally separate from ReadyQuery to avoid
// changing the cross-store Ready contract (and to avoid adding map/slice fields
// to ReadyQuery, which is compared by struct equality elsewhere).
type ControlReadyFilter struct {
	Assignee         string            // exact assignee match; ignored when Unassigned is true
	Unassigned       bool              // match beads with no assignee (Assignee == "")
	Metadata         map[string]string // AND-match on top-level bead metadata key=value
	ExcludeTypes     []string          // ADDITIONAL exclusions on top of the store's built-in readyExcludeTypes
	IncludeEphemeral bool              // include the ephemeral (wisps) tier
	Sort             SortOrder         // SortCreatedAsc == bd "--sort oldest"
	Limit            int
}

// ControlReadyQuerier is implemented only by stores that can answer a
// ControlReadyFilter in-process. Callers MUST type-assert and fall back to the
// shell query path when it is not implemented.
type ControlReadyQuerier interface {
	ControlReady(filter ControlReadyFilter) ([]Bead, error)
}
```

- [ ] **Step 4: Add the concrete compile assertion next to the existing one**

Modify `internal/beads/native_dolt_store.go:122` — after `var _ Store = (*NativeDoltStore)(nil)` add:

```go
var _ ControlReadyQuerier = (*NativeDoltStore)(nil)
```

(The package still won't build until `ControlReady` exists — that's Task 2. The two assertions now both reference real symbols, so the only remaining error is the missing method.)

- [ ] **Step 5: Run to confirm the only remaining error is the missing method**

Run: `go test ./internal/beads/ -run TestControlReady`
Expected: FAIL — build error `*NativeDoltStore does not implement ControlReadyQuerier (missing method ControlReady)`.

> Use `go test` (not `go build`): the assertion at `:122` is in a non-test file so
> `go build` would also catch it, but keeping the RED command consistent with the
> test file avoids confusion.

- [ ] **Step 6: Commit**

```bash
git add internal/beads/query.go internal/beads/native_dolt_store.go internal/beads/native_dolt_store_control_ready_test.go
git commit -m "feat(beads): add ControlReadyFilter + ControlReadyQuerier capability interface"
```

---

## Task 2: `NativeDoltStore.ControlReady` implementation (`internal/beads`)

**Files:**
- Modify: `internal/beads/native_dolt_store.go` (add method after `Ready`, ~`:462`)
- Test: `internal/beads/native_dolt_store_control_ready_test.go`

> The contract tests need real beads in a native store. Reuse the existing test
> harness used by other `native_dolt_store_*_test.go` files. Before writing,
> grep for a helper that opens a temp native store:
> `grep -n "func newTestNativeStore\|OpenNativeDoltStoreAt\|func.*nativeTestStore" internal/beads/*_test.go`.
> Use whatever constructor the existing native-store tests use; the snippets
> below assume a helper `newTestNativeStore(t)` returning `*NativeDoltStore` and
> a `seed(store, Bead{...})` pattern. **Match the existing tests' actual helper
> names** — adapt the calls, keep the assertions.

- [ ] **Step 1: Write the failing contract tests**

**Rewrite** `internal/beads/native_dolt_store_control_ready_test.go` as a complete
file (do NOT append an `import` block after the Task 1 `var _ = ...` declaration —
Go requires imports before declarations). Replace the whole file with:

```go
package beads

import (
	"testing"
	"time"
)

// Compile-time assertion that NativeDoltStore satisfies the new capability.
var _ ControlReadyQuerier = (*NativeDoltStore)(nil)

func TestControlReady_AssigneeMatch(t *testing.T) {
	s := newTestNativeStore(t)
	seed(t, s, Bead{ID: "a-1", Type: "task", Status: "open", Assignee: "ctrl"})
	seed(t, s, Bead{ID: "a-2", Type: "task", Status: "open", Assignee: "other"})

	got, err := s.ControlReady(ControlReadyFilter{Assignee: "ctrl", Limit: 50})
	if err != nil {
		t.Fatalf("ControlReady: %v", err)
	}
	if ids := idsOf(got); !equalIDs(ids, []string{"a-1"}) {
		t.Fatalf("assignee match: got %v, want [a-1]", ids)
	}
}

func TestControlReady_UnassignedMatch(t *testing.T) {
	s := newTestNativeStore(t)
	seed(t, s, Bead{ID: "u-1", Type: "task", Status: "open", Assignee: ""})
	seed(t, s, Bead{ID: "u-2", Type: "task", Status: "open", Assignee: "someone"})

	got, err := s.ControlReady(ControlReadyFilter{Unassigned: true, Limit: 50})
	if err != nil {
		t.Fatalf("ControlReady: %v", err)
	}
	if ids := idsOf(got); !equalIDs(ids, []string{"u-1"}) {
		t.Fatalf("unassigned match: got %v, want [u-1]", ids)
	}
}

func TestControlReady_MetadataAndMatch(t *testing.T) {
	s := newTestNativeStore(t)
	seed(t, s, Bead{ID: "m-1", Type: "task", Status: "open",
		Metadata: map[string]string{"gc.run_target": "ctrl"}})
	seed(t, s, Bead{ID: "m-2", Type: "task", Status: "open",
		Metadata: map[string]string{"gc.run_target": "elsewhere"}})

	got, err := s.ControlReady(ControlReadyFilter{
		Metadata:   map[string]string{"gc.run_target": "ctrl"},
		Unassigned: true, Limit: 50,
	})
	if err != nil {
		t.Fatalf("ControlReady: %v", err)
	}
	if ids := idsOf(got); !equalIDs(ids, []string{"m-1"}) {
		t.Fatalf("metadata match: got %v, want [m-1]", ids)
	}
}

func TestControlReady_ExcludeTypeEpicIsAdditive(t *testing.T) {
	s := newTestNativeStore(t)
	seed(t, s, Bead{ID: "e-task", Type: "task", Status: "open", Assignee: "ctrl"})
	seed(t, s, Bead{ID: "e-epic", Type: "epic", Status: "open", Assignee: "ctrl"})

	got, err := s.ControlReady(ControlReadyFilter{
		Assignee: "ctrl", ExcludeTypes: []string{"epic"}, Limit: 50,
	})
	if err != nil {
		t.Fatalf("ControlReady: %v", err)
	}
	if ids := idsOf(got); !equalIDs(ids, []string{"e-task"}) {
		t.Fatalf("exclude epic: got %v, want [e-task]", ids)
	}
}

func TestControlReady_IncludeEphemeralReturnsWisps(t *testing.T) {
	s := newTestNativeStore(t)
	seed(t, s, Bead{ID: "w-1", Type: "task", Status: "open", Assignee: "ctrl", Ephemeral: true})

	// Without IncludeEphemeral, the wisp must be excluded.
	off, err := s.ControlReady(ControlReadyFilter{Assignee: "ctrl", Limit: 50})
	if err != nil {
		t.Fatalf("ControlReady (no ephemeral): %v", err)
	}
	if ids := idsOf(off); len(ids) != 0 {
		t.Fatalf("ephemeral leaked without IncludeEphemeral: got %v", ids)
	}

	// With IncludeEphemeral, the wisp must appear (Finding 1: tier-aware).
	on, err := s.ControlReady(ControlReadyFilter{Assignee: "ctrl", IncludeEphemeral: true, Limit: 50})
	if err != nil {
		t.Fatalf("ControlReady (ephemeral): %v", err)
	}
	if ids := idsOf(on); !equalIDs(ids, []string{"w-1"}) {
		t.Fatalf("include-ephemeral: got %v, want [w-1]", ids)
	}
}

func TestControlReady_SortOldestAndLimit(t *testing.T) {
	s := newTestNativeStore(t)
	// Seed three with increasing created times; oldest first expected.
	seedAt(t, s, Bead{ID: "s-old", Type: "task", Status: "open", Assignee: "ctrl"}, time.Unix(100, 0))
	seedAt(t, s, Bead{ID: "s-mid", Type: "task", Status: "open", Assignee: "ctrl"}, time.Unix(200, 0))
	seedAt(t, s, Bead{ID: "s-new", Type: "task", Status: "open", Assignee: "ctrl"}, time.Unix(300, 0))

	got, err := s.ControlReady(ControlReadyFilter{
		Assignee: "ctrl", Sort: SortCreatedAsc, Limit: 2,
	})
	if err != nil {
		t.Fatalf("ControlReady: %v", err)
	}
	if ids := idsOf(got); !equalIDs(ids, []string{"s-old", "s-mid"}) {
		t.Fatalf("sort oldest + limit: got %v, want [s-old s-mid]", ids)
	}
}
```

Also add small local test helpers at the bottom of the file IF the package does
not already provide them (grep first: `grep -n "func idsOf\|func equalIDs" internal/beads/*_test.go`):

```go
func idsOf(bs []Bead) []string {
	ids := make([]string, 0, len(bs))
	for _, b := range bs {
		ids = append(ids, b.ID)
	}
	return ids
}

func equalIDs(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
```

- [ ] **Step 2: Run to verify the tests fail**

Run: `go test ./internal/beads/ -run TestControlReady -v`
Expected: FAIL — missing method `ControlReady` (compile error), then once the method stub exists, assertion failures.

- [ ] **Step 3: Implement `ControlReady`**

Add to `internal/beads/native_dolt_store.go` immediately after `Ready` (after `:462`):

```go
// ControlReady answers a single control-dispatcher ready sub-query in-process,
// mapping ControlReadyFilter onto beadslib.WorkFilter so GetReadyWork performs
// the unblocked/actionable/dependency filtering. Unlike Ready, it honors
// IncludeEphemeral (Finding 1) by not re-applying the non-ephemeral post-filter.
func (s *NativeDoltStore) ControlReady(filter ControlReadyFilter) ([]Bead, error) {
	storage, release, err := s.acquireStorage()
	if err != nil {
		return nil, err
	}
	defer release()
	ctx, cancel := nativeDoltOperationContext(context.TODO())
	defer cancel()

	excludeTypes := make([]beadslib.IssueType, 0, len(filter.ExcludeTypes))
	for _, t := range filter.ExcludeTypes {
		excludeTypes = append(excludeTypes, beadslib.IssueType(t))
	}

	var beads []Bead
	seen := make(map[string]bool)
	now := time.Now().UTC()
statusLoop:
	for _, status := range nativeDoltOpenReadyStatuses {
		wf := beadslib.WorkFilter{
			Status:           status,
			Unassigned:       filter.Unassigned,
			MetadataFields:   filter.Metadata,
			ExcludeTypes:     excludeTypes,
			IncludeEphemeral: filter.IncludeEphemeral,
			Limit:            filter.Limit,
		}
		if !filter.Unassigned && filter.Assignee != "" {
			wf.Assignee = &filter.Assignee
		}
		if filter.Sort == SortCreatedAsc {
			wf.SortPolicy = beadslib.SortPolicyOldest
		}

		issues, err := storage.GetReadyWork(ctx, wf)
		if err != nil {
			return nil, err
		}
		for _, issue := range issues {
			bead, err := beadFromNativeIssue(issue)
			if err != nil {
				return nil, err
			}
			if seen[bead.ID] || !isControlReadyCandidate(bead, now, filter.IncludeEphemeral) {
				continue
			}
			seen[bead.ID] = true
			beads = append(beads, bead)
			if filter.Limit > 0 && len(beads) >= filter.Limit {
				break statusLoop
			}
		}
	}
	return beads, nil
}
```

- [ ] **Step 4: Add the tier-aware candidate helper to `internal/beads/beads.go`**

After `IsReadyCandidate` (`beads.go:159`), add:

```go
// isControlReadyCandidate is the control-dispatcher variant of IsReadyCandidate
// that conditionally permits ephemeral beads (Finding 1). GetReadyWork already
// applies the server-side filters; this is a defense-in-depth post-filter that
// must NOT drop ephemeral beads when the caller asked to include them.
func isControlReadyCandidate(b Bead, now time.Time, includeEphemeral bool) bool {
	if b.Status != "open" {
		return false
	}
	if b.Ephemeral && !includeEphemeral {
		return false
	}
	return !IsReadyExcludedType(b.Type) && !IsDeferred(b, now)
}
```

> **Status-parity (review MAJOR, resolved):** `nativeDoltOpenReadyStatuses`
> (`native_dolt_store.go:19-27`) includes non-`open` workflow statuses
> (blocked/deferred/pinned/hooked/review/testing). `bd ready` (the shell path)
> queries `Status: open` only. We deliberately MIRROR the existing
> `NativeDoltStore.Ready` loop+post-filter structure (`:436`-`:461`) because
> `Ready` is gascity's *accepted, in-production* native equivalent of `bd ready` —
> `GetReadyWork` returns only unblocked/actionable work per status bucket, and
> `isControlReadyCandidate` (like `IsReadyCandidate`) post-filters to
> `Status == "open"`, so non-open buckets contribute nothing. Reproducing `Ready`'s
> exact shape (not inventing a single-status query) is the lowest-risk parity
> choice; the golden parity test (Task 6) is the hard backstop. This MUST be
> locked by a dedicated test (Step 4b below).

- [ ] **Step 4b: Add the required status-exclusion contract test**

Append to `internal/beads/native_dolt_store_control_ready_test.go`:

```go
func TestControlReady_ExcludesInProgressAndBlocked(t *testing.T) {
	s := newTestNativeStore(t)
	seed(t, s, Bead{ID: "st-open", Type: "task", Status: "open", Assignee: "ctrl"})
	seed(t, s, Bead{ID: "st-prog", Type: "task", Status: "in_progress", Assignee: "ctrl"})
	// A bead with an open blocker is not "ready" (GetReadyWork excludes it); seed
	// it via the harness's blocked-dependency helper if available, else assert
	// only the in_progress exclusion. Match bd ready: only st-open is returned.

	got, err := s.ControlReady(ControlReadyFilter{Assignee: "ctrl", IncludeEphemeral: true, Limit: 50})
	if err != nil {
		t.Fatalf("ControlReady: %v", err)
	}
	if ids := idsOf(got); !equalIDs(ids, []string{"st-open"}) {
		t.Fatalf("status exclusion: got %v, want [st-open]", ids)
	}
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/beads/ -run TestControlReady -v`
Expected: PASS (all sub-tests).

- [ ] **Step 6: Run the full beads package to ensure no regressions**

Run: `go test ./internal/beads/...`
Expected: PASS. (Confirms `Ready` and struct-equality at `caching_store_reads.go:331` are untouched.)

- [ ] **Step 7: Commit**

```bash
git add internal/beads/native_dolt_store.go internal/beads/beads.go internal/beads/native_dolt_store_control_ready_test.go
git commit -m "feat(beads): implement NativeDoltStore.ControlReady in-process ready fast path"
```

---

## Task 3: Candidate/route derivation helper (`cmd/gc`)

Mirrors the shell loop's id resolution so the in-process path selects exactly the
same candidates and routes.

**Files:**
- Modify: `cmd/gc/dispatch_runtime.go` (add `deriveControlReadyTargets`)
- Test: `cmd/gc/dispatch_control_ready_test.go` (create)

- [ ] **Step 1: Write the failing test**

Create `cmd/gc/dispatch_control_ready_test.go`:

```go
package main

import "testing"

func TestDeriveControlReadyTargets_OrderAndLegacyExpansion(t *testing.T) {
	env := map[string]string{
		"GC_CONTROL_SESSION_NAME": "rig/control-dispatcher",
		"GC_SESSION_NAME":         "rig/control-dispatcher",
		"GC_ALIAS":                "ctrl-alias",
		"GC_CONTROL_TARGET":       "rig/control-dispatcher",
		"GC_SESSION_ID":           "sess-123",
	}
	cands, routes := deriveControlReadyTargets(env)

	// Loop order: SESSION_NAME, SESSION_NAME(dup), ALIAS, CONTROL_TARGET, SESSION_ID.
	// Each control-dispatcher id appends its legacy "workflow-control" variant.
	// NO pre-dedup (parity with shell; dedup happens after the ready merge).
	want := []string{
		"rig/control-dispatcher", "rig/workflow-control", // GC_CONTROL_SESSION_NAME + legacy
		"rig/control-dispatcher", "rig/workflow-control", // GC_SESSION_NAME + legacy
		"ctrl-alias",                                      // GC_ALIAS (no legacy)
		"rig/control-dispatcher", "rig/workflow-control", // GC_CONTROL_TARGET + legacy
		"sess-123",                                        // GC_SESSION_ID (no legacy)
	}
	if !equalStrs(cands, want) {
		t.Fatalf("candidates:\n got  %v\n want %v", cands, want)
	}

	// Routes: GC_CONTROL_TARGET then GC_CONTROL_LEGACY_TARGET (derived).
	wantRoutes := []string{"rig/control-dispatcher", "rig/workflow-control"}
	if !equalStrs(routes, wantRoutes) {
		t.Fatalf("routes:\n got  %v\n want %v", routes, wantRoutes)
	}
}

func TestDeriveControlReadyTargets_SkipsEmpties(t *testing.T) {
	env := map[string]string{
		"GC_CONTROL_TARGET": "plain-target",
		// all other id vars empty/absent
	}
	cands, routes := deriveControlReadyTargets(env)
	if !equalStrs(cands, []string{"plain-target"}) {
		t.Fatalf("candidates: got %v, want [plain-target]", cands)
	}
	// plain-target does not end in control-dispatcher → no legacy route.
	if !equalStrs(routes, []string{"plain-target"}) {
		t.Fatalf("routes: got %v, want [plain-target]", routes)
	}
}

func equalStrs(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./cmd/gc/ -run TestDeriveControlReadyTargets -v`
Expected: FAIL — `undefined: deriveControlReadyTargets`.

- [ ] **Step 3: Implement the helper**

Add to `cmd/gc/dispatch_runtime.go` (near `workflowServeControlReadyQuery`, after `:728`):

```go
// controlReadyIDEnvVars is the candidate-id resolution order, matching the shell
// loop in workflowServeControlReadyQuery exactly.
var controlReadyIDEnvVars = []string{
	"GC_CONTROL_SESSION_NAME",
	"GC_SESSION_NAME",
	"GC_ALIAS",
	"GC_CONTROL_TARGET",
	"GC_SESSION_ID",
}

// deriveControlReadyTargets reproduces the shell query's candidate-id and route
// derivation in-process. It returns candidates in loop order WITHOUT pre-dedup
// (the shell merges then dedups first-occurrence-wins), each control-dispatcher
// id followed by its legacy "workflow-control" variant; and routes as
// [GC_CONTROL_TARGET, GC_CONTROL_LEGACY_TARGET] (empties skipped).
func deriveControlReadyTargets(env map[string]string) (candidates, routes []string) {
	for _, key := range controlReadyIDEnvVars {
		id := strings.TrimSpace(env[key])
		if id == "" {
			continue
		}
		candidates = append(candidates, id)
		if legacy := controlReadyLegacyCandidate(id); legacy != "" {
			candidates = append(candidates, legacy)
		}
	}
	if target := strings.TrimSpace(env["GC_CONTROL_TARGET"]); target != "" {
		routes = append(routes, target)
	}
	if legacy := strings.TrimSpace(env["GC_CONTROL_LEGACY_TARGET"]); legacy != "" {
		routes = append(routes, legacy)
	}
	return candidates, routes
}

// controlReadyLegacyCandidate mirrors the shell `case "$id" in *control-dispatcher)`
// expansion: id ending in "control-dispatcher" → "<prefix>workflow-control".
func controlReadyLegacyCandidate(id string) string {
	const suffix = "control-dispatcher"
	if strings.HasSuffix(id, suffix) {
		return strings.TrimSuffix(id, suffix) + "workflow-control"
	}
	return ""
}
```

> **Parity check the implementer MUST verify against the shell** (`:716`-`:725`):
> the shell uses the *resolved* env values. For the in-process path, `env` must be
> the same merged env the shell sees — i.e. `workEnv` overlaid on `os.Environ()`,
> with the `GC_CONTROL_TARGET`/`GC_CONTROL_SESSION_NAME`/`GC_CONTROL_LEGACY_TARGET`
> prefix overrides applied (those are baked into the shell query prefix at
> `:697`-`:708`). Task 5 constructs this merged env; this helper only reads it.

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./cmd/gc/ -run TestDeriveControlReadyTargets -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/gc/dispatch_runtime.go cmd/gc/dispatch_control_ready_test.go
git commit -m "feat(cmd/gc): add in-process control-ready candidate/route derivation"
```

---

## Task 4: Fan-out helper `controlDispatcherReadyBeads` (`cmd/gc`)

**Files:**
- Modify: `cmd/gc/dispatch_runtime.go`
- Test: `cmd/gc/dispatch_control_ready_test.go`

- [ ] **Step 1: Write the failing tests with a fake `ControlReadyQuerier`**

Append to `cmd/gc/dispatch_control_ready_test.go`:

```go
import "github.com/.../internal/beads" // use the repo's actual module path; grep an existing import

// fakeControlReadyQuerier records calls and returns scripted results keyed by a
// canonical filter signature.
type fakeControlReadyQuerier struct {
	calls   []beads.ControlReadyFilter
	results map[string][]beads.Bead
	errOn   map[string]bool
}

func (f *fakeControlReadyQuerier) ControlReady(filter beads.ControlReadyFilter) ([]beads.Bead, error) {
	f.calls = append(f.calls, filter)
	key := controlReadyFilterKey(filter)
	if f.errOn[key] {
		return nil, fmt.Errorf("boom: %s", key)
	}
	return f.results[key], nil
}

func TestControlDispatcherReadyBeads_FanOutNoPreDedupFirstOccurrenceWins(t *testing.T) {
	f := &fakeControlReadyQuerier{results: map[string][]beads.Bead{
		"assignee=c1":          {{ID: "b-shared"}, {ID: "b-1"}},
		"assignee=c2":          {{ID: "b-shared"}, {ID: "b-2"}}, // b-shared is a dup
		"unassigned;gc.run_target=r1": {{ID: "b-3"}},
		"unassigned;gc.routed_to=r1":  {{ID: "b-3"}}, // dup again
	}}

	got, err := controlDispatcherReadyBeads(f, []string{"c1", "c2"}, []string{"r1"}, 50)
	if err != nil {
		t.Fatalf("controlDispatcherReadyBeads: %v", err)
	}
	// First-occurrence-wins ordering, deduped by id.
	wantIDs := []string{"b-shared", "b-1", "b-2", "b-3"}
	if ids := hookIDs(got); !equalStrs(ids, wantIDs) {
		t.Fatalf("merge order: got %v, want %v", ids, wantIDs)
	}
}

func TestControlDispatcherReadyBeads_SoftFailContinues(t *testing.T) {
	f := &fakeControlReadyQuerier{
		results: map[string][]beads.Bead{"assignee=c2": {{ID: "b-ok"}}},
		errOn:   map[string]bool{"assignee=c1": true},
	}
	got, err := controlDispatcherReadyBeads(f, []string{"c1", "c2"}, nil, 50)
	if err != nil {
		t.Fatalf("soft-fail must not surface a single sub-query error: %v", err)
	}
	if ids := hookIDs(got); !equalStrs(ids, []string{"b-ok"}) {
		t.Fatalf("soft-fail: got %v, want [b-ok]", ids)
	}
}

func TestControlDispatcherReadyBeads_AllFailReturnsEmptyNoError(t *testing.T) {
	// Soft-fail parity: shell always exits 0 with `[]` even when every bd call
	// fails. The in-process path must do the same (never error) so the serve loop
	// does not fall back to shell on a transient native hiccup.
	f := &fakeControlReadyQuerier{errOn: map[string]bool{"assignee=c1": true}}
	got, err := controlDispatcherReadyBeads(f, []string{"c1"}, nil, 50)
	if err != nil {
		t.Fatalf("all-fail must not surface an error (shell parity): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("all-fail: got %v, want empty", hookIDs(got))
	}
}

func TestControlDispatcherReadyBeads_EmptyInputsReturnNilNil(t *testing.T) {
	f := &fakeControlReadyQuerier{}
	got, err := controlDispatcherReadyBeads(f, nil, nil, 50)
	if err != nil || got != nil {
		t.Fatalf("empty inputs: got (%v, %v), want (nil, nil)", got, err)
	}
}

// hookIDs and controlReadyFilterKey are test helpers; controlReadyFilterKey must
// match the production filter shapes the implementer chooses. Adjust the result
// keys above if production uses different canonical strings.
func hookIDs(hs []hookBead) []string {
	ids := make([]string, 0, len(hs))
	for _, h := range hs {
		ids = append(ids, h.ID)
	}
	return ids
}

func controlReadyFilterKey(f beads.ControlReadyFilter) string {
	if f.Unassigned {
		for k, v := range f.Metadata {
			return "unassigned;" + k + "=" + v
		}
		return "unassigned"
	}
	return "assignee=" + f.Assignee
}
```

> The implementer should make the production fan-out emit filters whose
> `controlReadyFilterKey` matches the scripted keys, or adjust the test keys to
> the production filter shapes. The behavioral assertions (order, dedup,
> soft-fail, empty) are the contract; the key strings are an implementation seam.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./cmd/gc/ -run TestControlDispatcherReadyBeads -v`
Expected: FAIL — `undefined: controlDispatcherReadyBeads`.

- [ ] **Step 3: Implement the fan-out helper**

Add to `cmd/gc/dispatch_runtime.go`:

```go
// controlDispatcherReadyBeads runs the control-dispatcher ready fan-out in-process,
// mirroring workflowServeControlReadyQuery's shell semantics exactly:
//   - per candidate (in order, no pre-dedup): assignee ready, include-ephemeral,
//     exclude-type=epic, limit=limit;
//   - per route: two unassigned metadata queries (gc.run_target / gc.routed_to),
//     include-ephemeral, exclude-type=epic, sort oldest, limit=limit;
//   - per-subquery soft-fail (matches shell `|| true` and the final `|| printf "[]"`):
//     log+skip a failing sub-query AND never surface an error to the caller — the
//     shell script ALWAYS exits 0 with valid JSON (`[]` on total failure). True
//     parity therefore means controlDispatcherReadyBeads never returns a non-nil
//     error; sub-query failures only reduce results;
//   - concatenate in order, dedup by id keeping first occurrence.
// Returns (nil, nil) when there is no work (matches shell `[]`).
func controlDispatcherReadyBeads(q beads.ControlReadyQuerier, candidates, routes []string, limit int) ([]hookBead, error) {
	var merged []hookBead
	seen := make(map[string]bool)

	emit := func(filter beads.ControlReadyFilter) {
		got, err := q.ControlReady(filter)
		if err != nil {
			// soft-fail: skip this sub-query, mirroring shell `2>/dev/null || true`.
			return
		}
		for _, b := range got {
			if seen[b.ID] {
				continue
			}
			seen[b.ID] = true
			merged = append(merged, hookBead{ID: b.ID, Metadata: hookBeadMetadata(b.Metadata)})
		}
	}

	for _, cand := range candidates {
		emit(beads.ControlReadyFilter{
			Assignee: cand, IncludeEphemeral: true,
			ExcludeTypes: []string{"epic"}, Limit: limit,
		})
	}
	for _, route := range routes {
		for _, key := range []string{"gc.run_target", "gc.routed_to"} {
			emit(beads.ControlReadyFilter{
				Unassigned: true, Metadata: map[string]string{key: route},
				IncludeEphemeral: true, ExcludeTypes: []string{"epic"},
				Sort: beads.SortCreatedAsc, Limit: limit,
			})
		}
	}

	// Always (nil, nil) on no results — the shell path returns `[]` and exits 0
	// even when every `bd` call failed, so the serve loop must NOT fall back to
	// shell on a transient native hiccup (that would reintroduce execs). The
	// error return remains in the signature for forward-compat but is never set.
	return merged, nil
}
```

> **Review note (soft-fail parity, MAJOR):** the prior draft returned an error
> when every sub-query failed; both reviewers flagged that the shell path never
> errors (`|| true` + `|| printf "[]"`). This version matches shell exactly:
> failures only shrink results, never error. Update the Task 4 test accordingly —
> `TestControlDispatcherReadyBeads_AllFailReturnsError` is REPLACED by
> `TestControlDispatcherReadyBeads_AllFailReturnsEmptyNoError` (see Step 1 update
> below).

> Verify `beads.Bead.Metadata` is a `map[string]string` so the conversion to
> `hookBeadMetadata` compiles; if it's a different type, map keys/values
> explicitly. Grep: `grep -n "Metadata" internal/beads/beads.go | head`.

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./cmd/gc/ -run TestControlDispatcherReadyBeads -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/gc/dispatch_runtime.go cmd/gc/dispatch_control_ready_test.go
git commit -m "feat(cmd/gc): add in-process control-dispatcher ready fan-out with soft-fail + first-occurrence dedup"
```

---

## Task 5: Serve-seam wiring (`cmd/gc/dispatch_runtime.go`)

Branch in `drainWorkflowServeWork`: when the agent is the control-dispatcher AND a
lazily-opened store implements `beads.ControlReadyQuerier`, run the in-process
path; otherwise fall through to the unchanged `workflowServeList` shell call.

**Files:**
- Modify: `cmd/gc/dispatch_runtime.go` (`drainWorkflowServeWork`, `:433`-`:438`; add injectable opener var)
- Test: `cmd/gc/dispatch_control_ready_test.go`

- [ ] **Step 1: Write the failing seam-selection test**

Append to `cmd/gc/dispatch_control_ready_test.go`:

```go
func TestControlReadyServeSelection_UsesInProcessWhenCapable(t *testing.T) {
	// Inject a store opener returning a capable fake; assert the shell path
	// (workflowServeList) is NOT called for the control-dispatcher agent.
	prevOpener := controlReadyStoreOpener
	prevList := workflowServeList
	t.Cleanup(func() { controlReadyStoreOpener = prevOpener; workflowServeList = prevList })

	fake := &fakeControlReadyQuerier{results: map[string][]beads.Bead{
		"assignee=rig/control-dispatcher": {{ID: "in-proc-1"}},
	}}
	controlReadyStoreOpener = func(storePath, cityPath string) (beads.ControlReadyQuerier, bool) {
		return fake, true
	}
	shellCalled := false
	workflowServeList = func(workQuery, dir string, env map[string]string) ([]hookBead, error) {
		shellCalled = true
		return nil, nil
	}

	got := serveControlReadyOrShell(
		controlDispatcherAgentCfgForTest(t), // helper returning a control-dispatcher config.Agent
		"city", "store", "serveQuery",
		map[string]string{"GC_CONTROL_TARGET": "rig/control-dispatcher"},
		io.Discard,
	)
	if shellCalled {
		t.Fatalf("shell path must not run when in-process capability is present")
	}
	if ids := hookIDs(got.queue); !equalStrs(ids, []string{"in-proc-1"}) {
		t.Fatalf("in-process selection: got %v, want [in-proc-1]", ids)
	}
}

func TestControlReadyServeSelection_FallsBackToShellWhenIncapable(t *testing.T) {
	prevOpener := controlReadyStoreOpener
	prevList := workflowServeList
	t.Cleanup(func() { controlReadyStoreOpener = prevOpener; workflowServeList = prevList })

	controlReadyStoreOpener = func(storePath, cityPath string) (beads.ControlReadyQuerier, bool) {
		return nil, false // not capable (e.g. BdStore)
	}
	shellCalled := false
	workflowServeList = func(workQuery, dir string, env map[string]string) ([]hookBead, error) {
		shellCalled = true
		return []hookBead{{ID: "shell-1"}}, nil
	}

	got := serveControlReadyOrShell(
		controlDispatcherAgentCfgForTest(t),
		"city", "store", "serveQuery",
		map[string]string{"GC_CONTROL_TARGET": "rig/control-dispatcher"},
		io.Discard,
	)
	if !shellCalled {
		t.Fatalf("shell path must run when capability is absent")
	}
	if ids := hookIDs(got.queue); !equalStrs(ids, []string{"shell-1"}) {
		t.Fatalf("fallback: got %v, want [shell-1]", ids)
	}
}
```

> `controlDispatcherAgentCfgForTest` should return a `config.Agent` whose
> `QualifiedName()` satisfies `isWorkflowServeControlDispatcherAgent`. Grep
> existing tests for how control-dispatcher agent configs are built:
> `grep -rn "ControlDispatcherAgentName\|control-dispatcher" cmd/gc/*_test.go | head`.
> The exact `serveControlReadyOrShell` return type (`got.queue`) is an
> implementation choice — Step 3 defines it; adjust the test field access to match.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./cmd/gc/ -run TestControlReadyServeSelection -v`
Expected: FAIL — `undefined: controlReadyStoreOpener` / `serveControlReadyOrShell`.

- [ ] **Step 3: Implement the injectable opener + selection function, and wire the seam**

Add to `cmd/gc/dispatch_runtime.go`:

```go
// controlReadyStoreOpener opens a store for the given paths and reports whether
// it can answer control-ready queries in-process. Swappable for tests. The
// production implementation reuses the same path gc status uses to select the
// store, so eligibility exactly matches reported beads_store.
//
// MAJOR review fix (store churn): the serve loop calls this on EVERY drain
// iteration (drainWorkflowServeWork inner loop, dispatch_runtime.go:436). Opening
// a fresh native store each poll re-introduces per-cycle churn — the exact thing
// we are eliminating. The production opener therefore MEMOIZES by (storePath,
// cityPath): it opens once and reuses the handle for the lifetime of the serve
// process (which is bound to a single fixed scope). Tests still swap the var with
// their own func, so memoization is invisible to them.
var controlReadyStoreOpener = newCachingControlReadyOpener()

func newCachingControlReadyOpener() func(storePath, cityPath string) (beads.ControlReadyQuerier, bool) {
	var (
		mu        sync.Mutex
		cachedKey string
		cachedQ   beads.ControlReadyQuerier
		cachedOK  bool
		done      bool
	)
	return func(storePath, cityPath string) (beads.ControlReadyQuerier, bool) {
		mu.Lock()
		defer mu.Unlock()
		key := storePath + "\x00" + cityPath
		if done && key == cachedKey {
			return cachedQ, cachedOK
		}
		store, err := openStoreAtForCity(storePath, cityPath)
		if err != nil {
			cachedKey, cachedQ, cachedOK, done = key, nil, false, true
			return nil, false
		}
		q, ok := store.(beads.ControlReadyQuerier)
		cachedKey, cachedQ, cachedOK, done = key, q, ok, true
		return q, ok
	}
}

// serveControlReadyResult carries the queue plus whether the in-process path ran
// (for tracing/metrics).
type serveControlReadyResult struct {
	queue     []hookBead
	inProcess bool
	err       error
}

// serveControlReadyOrShell returns the control-dispatcher ready queue, preferring
// the in-process capability and falling back to the shell query var otherwise.
func serveControlReadyOrShell(agentCfg config.Agent, cityPath, storePath, serveQuery string, workEnv map[string]string, stderr io.Writer) serveControlReadyResult {
	if isWorkflowServeControlDispatcherAgent(agentCfg) {
		if q, ok := controlReadyStoreOpener(storePath, cityPath); ok {
			candidates, routes := deriveControlReadyTargets(controlReadyResolvedEnv(agentCfg, workEnv))
			queue, err := controlDispatcherReadyBeads(q, candidates, routes, workflowServeScanLimit)
			if err == nil {
				return serveControlReadyResult{queue: queue, inProcess: true}
			}
			// On in-process error, fall through to the shell path (safety).
			fmt.Fprintf(stderr, "control-ready in-process path failed, falling back to shell: %v\n", err)
		}
	}
	queue, err := workflowServeList(serveQuery, storePath, workEnv)
	return serveControlReadyResult{queue: queue, err: err}
}

// controlReadyResolvedEnv reproduces the EXACT env the shell query sees. CRITICAL
// review fix (C1): GC_CONTROL_TARGET / GC_CONTROL_SESSION_NAME /
// GC_CONTROL_LEGACY_TARGET are NOT present in workEnv or os.Environ — the shell
// path bakes them into its command prefix (workflowServeControlReadyQuery,
// dispatch_runtime.go:697-708). We must inject the same values here, mirroring
// that prefix construction exactly, or deriveControlReadyTargets returns a strict
// subset of candidates/routes and the in-process path silently diverges.
func controlReadyResolvedEnv(agentCfg config.Agent, workEnv map[string]string) map[string]string {
	env := envSliceToMap(mergeRuntimeEnv(os.Environ(), workEnv))

	target := strings.TrimSpace(agentCfg.QualifiedName())
	if target == "" {
		target = config.ControlDispatcherAgentName
	}
	env["GC_CONTROL_TARGET"] = target

	// GC_CONTROL_SESSION_NAME: the shell builder sets this from its
	// controlSessionNames variadic (first non-empty). At the serve seam the
	// equivalent runtime session name is what runWorkflowServe passes when it
	// builds the shell query (dispatch_runtime.go:327). The implementer MUST use
	// that SAME source value here. If no explicit session name is threaded to the
	// seam, leave GC_CONTROL_SESSION_NAME to whatever is already in env (the shell
	// builder also omits it when empty — see the `break` on first non-empty at
	// :703-704). Do NOT invent a value. Verify against :325-:331.
	if legacy := workflowServeLegacyControlRoute(target); legacy != "" {
		env["GC_CONTROL_LEGACY_TARGET"] = legacy
	}
	return env
}
```

> **Implementer MUST verify (C1 backstop):** read `runWorkflowServe`
> `dispatch_runtime.go:316`-`:331` to see exactly what session-name value (if any)
> is passed to `workflowServeControlReadyQuery`/`workflowServeWorkQuery` when the
> shell query is built, and set `GC_CONTROL_SESSION_NAME` in
> `controlReadyResolvedEnv` from that identical source. The golden parity test
> (Task 6) is the hard gate: if `GC_CONTROL_SESSION_NAME` derivation differs, the
> candidate lists will not match and Task 6 fails.

Then change the seam in `drainWorkflowServeWork` (`:437`-`:438`) from:

```go
		serveQuery := workflowServeWorkQuery(agentCfg, workQuery)
		queue, err := workflowServeList(serveQuery, storePath, workEnv)
```

to:

```go
		serveQuery := workflowServeWorkQuery(agentCfg, workQuery)
		res := serveControlReadyOrShell(agentCfg, cityPath, storePath, serveQuery, workEnv, stderr)
		queue, err := res.queue, res.err
```

> `envSliceToMap` may already exist — grep first:
> `grep -n "func envSliceToMap\|func mergeRuntimeEnv" cmd/gc/*.go`. If absent, add a
> trivial `[]string ("K=V") → map[string]string` converter (last-wins) next to
> `mergeRuntimeEnv`. The C1 env-injection is now handled by `controlReadyResolvedEnv`
> in Step 3 (not a note). Add `"sync"` and `"strings"` to the `cmd/gc/dispatch_runtime.go`
> import block if not already present (the file already imports `strings`).

- [ ] **Step 4: Run the seam tests**

Run: `go test ./cmd/gc/ -run TestControlReadyServeSelection -v`
Expected: PASS.

- [ ] **Step 5: Run all existing serve/control-ready tests to confirm no regression**

Run: `go test ./cmd/gc/ -run "TestWorkflowServe|TestControlReady|TestDeriveControl|TestControlDispatcherReadyBeads" -v`
Expected: PASS — including the pre-existing `TestWorkflowServeControlReadyQuery*` tests (the shell builder and `workflowServeList` signature are unchanged).

- [ ] **Step 6: Commit**

```bash
git add cmd/gc/dispatch_runtime.go cmd/gc/dispatch_control_ready_test.go
git commit -m "feat(cmd/gc): branch control-dispatcher serve loop to in-process ready when store is native"
```

---

## Task 6: Golden parity test (in-process vs shell)

Proves the two paths return the same id list/order for identical fixture data —
the central anti-drift guard.

**Files:**
- Test: `cmd/gc/dispatch_control_ready_test.go`

- [ ] **Step 1: Write the golden parity test**

Append to `cmd/gc/dispatch_control_ready_test.go`. Seed a real native store with a
fixture set spanning every selection dimension (assignee match, unassigned+routed
metadata, ephemeral, epic-excluded, in-progress-excluded, an overlapping bead to
exercise dedup). Run BOTH paths and assert identical ordered id lists:

```go
func TestControlReady_GoldenParity_InProcessEqualsShell(t *testing.T) {
	if testing.Short() {
		t.Skip("golden parity uses a real native store + bd shell; skip in -short")
	}
	store := newTestNativeStoreOnDisk(t) // a native store whose storePath the shell `bd` can also read
	seedControlReadyParityFixture(t, store) // shared fixture: see helper below

	env := map[string]string{
		"GC_CONTROL_TARGET":       "rig/control-dispatcher",
		"GC_CONTROL_SESSION_NAME": "rig/control-dispatcher",
		"GC_CONTROL_LEGACY_TARGET": "rig/workflow-control",
		"GC_SESSION_NAME":         "rig/control-dispatcher",
	}

	// In-process path.
	candidates, routes := deriveControlReadyTargets(env)
	inProc, err := controlDispatcherReadyBeads(store, candidates, routes, workflowServeScanLimit)
	if err != nil {
		t.Fatalf("in-process: %v", err)
	}

	// Shell path: build the verbatim shell query and run it via the same runner.
	agentCfg := controlDispatcherAgentCfgForTest(t)
	shellQuery := workflowServeControlReadyQuery(agentCfg, "rig/control-dispatcher")
	shell, err := nextWorkflowServeBeads(shellQuery, store.StorePath(), env)
	if err != nil {
		t.Fatalf("shell: %v", err)
	}

	if a, b := hookIDs(inProc), hookIDs(shell); !equalStrs(a, b) {
		t.Fatalf("parity drift:\n in-process %v\n shell      %v", a, b)
	}
}
```

> This test requires the `bd`/`jq` binaries on PATH and a native store the shell
> can read at a real `storePath`. If the existing native test harness is in-memory
> only, gate this test behind an env guard (e.g. `if os.Getenv("GC_PARITY_E2E") ==
> ""` skip) and document running it in the verification step. The fixture helper
> `seedControlReadyParityFixture` MUST seed beads matching `assignee=rig/control-dispatcher`,
> `assignee=rig/workflow-control`, unassigned `gc.run_target=rig/control-dispatcher`,
> unassigned `gc.routed_to=rig/workflow-control`, one ephemeral, one epic (excluded),
> one in-progress (excluded), and one bead matching two sub-queries (dedup).

- [ ] **Step 2: Run the parity test**

Run: `GC_PARITY_E2E=1 go test ./cmd/gc/ -run TestControlReady_GoldenParity -v`
Expected: PASS (id lists identical). If it fails, the diff localizes the parity
bug (ordering, dedup, candidate derivation, or a filter mismatch) — fix the
in-process path, not the shell path.

- [ ] **Step 3: Commit**

```bash
git add cmd/gc/dispatch_control_ready_test.go
git commit -m "test(cmd/gc): golden parity test for in-process vs shell control-ready"
```

---

## Task 7: Full build, local supervisor verification, cross-build

**Files:** none (verification only)

- [ ] **Step 1: Full test + vet**

Run: `go build ./... && go vet ./cmd/gc/... ./internal/beads/... && go test ./internal/beads/... ./cmd/gc/...`
Expected: all PASS.

- [ ] **Step 2: Build the Mac binary**

Run: `go build -o dist/gc-darwin ./cmd/gc`
Expected: builds clean.

- [ ] **Step 3: Run the local Mac supervisor and confirm zero per-cycle execs**

Restart the local supervisor with the new binary, then during a control-dispatcher
serve cycle confirm **0** `bd` child processes and **0** `dolt remote -v` shell-outs:

```bash
# in one shell: watch for bd/dolt remote children spawned by gc
while true; do ps -A -o comm= | grep -E '^(bd|dolt)$' >/dev/null && echo "SPAWN $(date)"; sleep 1; done
```

Expected: no `SPAWN` lines attributable to the control-dispatcher serve loop while
the native store is active; ready selection unchanged (agents still get work).

- [ ] **Step 4: Cross-build linux/amd64 for the VM**

Run: `GOOS=linux GOARCH=amd64 go build -o dist/gc-linux-amd64 ./cmd/gc`
Expected: builds clean.

- [ ] **Step 5: Final commit (if dist/ is tracked; otherwise skip — dist/ is untracked)**

Confirm `git status`. `dist/` is untracked per repo convention — do NOT commit
binaries. Ensure all source commits from Tasks 1-6 are present:

```bash
git log --oneline origin/main..HEAD
```

Expected: the feature commits listed, working tree clean except untracked `dist/`.

---

## Self-Review (completed during planning)

- **Spec coverage:** Goals 1-3 → Tasks 2 (in-process), 3+4+6 (parity), 5 (fallback). All 7 design-review findings: F1 ephemeral (Task 2 Step 4 `isControlReadyCandidate` + test), F2 blast radius (Task 1 capability interface), F3 struct-equality (no `ReadyQuery` change; Task 2 Step 6 regression run), F4 soft-fail (Task 4), F5 ordering/limit (Tasks 3/4 tests), F6 wiring seam (Task 5), F7 ExcludeTypes additive/Unassigned/in-progress (Task 2 tests + helper).
- **Placeholder scan:** No TBDs; every code step has concrete code. The few `grep-first` notes are deliberate harness-name reconciliations (existing test helper names), not behavioral gaps — the behavioral contracts and assertions are fully specified.
- **Type consistency:** `ControlReadyFilter`/`ControlReadyQuerier`/`ControlReady`/`controlDispatcherReadyBeads`/`deriveControlReadyTargets`/`controlReadyStoreOpener`/`serveControlReadyOrShell`/`isControlReadyCandidate` used consistently across tasks. `SortCreatedAsc`→`SortPolicyOldest`, `hookBead{ID,Metadata}`, `WorkFilter` fields all match researched signatures.

## Open implementer reconciliations (cheap, mechanical)

1. Repo module import path for `internal/beads` in `cmd/gc` test (grep an existing import).
2. Native test-store harness helper names (`newTestNativeStore`/`seed`/`seedAt`/on-disk variant + `StorePath()`).
3. Whether `envSliceToMap`, `idsOf`, `equalIDs` already exist (avoid redeclaration).
4. `beads.Bead.Metadata` concrete type for the `hookBeadMetadata` conversion.
5. `beads.Bead.Metadata` is `map[string]string` (confirmed `beads.go:44`) → direct `hookBeadMetadata(...)` conversion compiles.
6. `GC_CONTROL_SESSION_NAME` source value at the serve seam — match `runWorkflowServe` `:325-:331` exactly (Task 5 Step 3 note). Golden parity test (Task 6) is the hard backstop.

## Round-2 plan-review resolutions (opus + codex, both NEEDS-CHANGES → addressed)

Both reviewers verified the plan against real source and converged. Confirmed-good
(no change needed): `beadslib.WorkFilter` honors `Assignee`/`Unassigned`/
`IncludeEphemeral`/`ExcludeTypes`/`MetadataFields`/`SortPolicy`/`Limit` together
(`issueops/ready_work.go`); the `List` "sort disables limit" quirk
(`native_dolt_store.go:1029-1033`) does NOT apply to `GetReadyWork`; `ReadyQuery`
untouched so `caching_store_reads.go:331` struct-equality is safe; non-control
agents unaffected (gated by `isWorkflowServeControlDispatcherAgent`); candidate/
route derivation parity table all green except the env-var availability bug below.

Fixes applied to this plan:

1. **CRITICAL — env parity (C1):** `GC_CONTROL_TARGET`/`GC_CONTROL_SESSION_NAME`/
   `GC_CONTROL_LEGACY_TARGET` are baked into the shell prefix only
   (`dispatch_runtime.go:697-708`), absent from `workEnv`/`os.Environ`
   (`work_query_probe.go:52-81`). Added `controlReadyResolvedEnv` (Task 5 Step 3)
   that injects them before `deriveControlReadyTargets` — now in the code, not a note.
2. **MAJOR — store churn:** `controlReadyStoreOpener` now memoizes by
   `(storePath,cityPath)` (`newCachingControlReadyOpener`) so the store opens once,
   not per drain iteration — preserving the optimization.
3. **MAJOR — status parity:** documented rationale for mirroring the proven `Ready`
   loop + added required `TestControlReady_ExcludesInProgressAndBlocked` (Task 2 Step 4b).
4. **MAJOR — soft-fail parity:** `controlDispatcherReadyBeads` now NEVER errors
   (shell always exits 0 with `[]`); test updated to
   `TestControlDispatcherReadyBeads_AllFailReturnsEmptyNoError`.
5. **MAJOR — RED step:** Task 1 RED uses `go test` (not `go build`, which skips `_test.go`).
6. **MINOR — file-order trap:** Task 2 Step 1 rewrites the test file as a complete
   unit (imports before declarations) instead of appending an import block.
