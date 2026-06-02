package main

import (
	"fmt"
	"io"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

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
		"ctrl-alias",                                     // GC_ALIAS (no legacy)
		"rig/control-dispatcher", "rig/workflow-control", // GC_CONTROL_TARGET + legacy
		"sess-123", // GC_SESSION_ID (no legacy)
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
		"assignee=c1":                 {{ID: "b-shared"}, {ID: "b-1"}},
		"assignee=c2":                 {{ID: "b-shared"}, {ID: "b-2"}}, // b-shared is a dup
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

// TestControlReady_GoldenPipeline_DeriveThenFanOut is the anti-drift golden for
// the cmd/gc-owned orchestration: it ties deriveControlReadyTargets (Task 3) to
// controlDispatcherReadyBeads (Task 4) as a single pipeline over a realistic
// control-dispatcher env, and pins the exact ordered, deduped id list.
//
// Parity layering (why this needs no shell bd / real dolt): the store-side filter
// semantics (ControlReady == bd ready for assignee / unassigned+metadata /
// exclude-epic / include-ephemeral / exclude-in-progress / sort+limit) are guarded
// in internal/beads (TestControlReady_* mirror the proven Ready path, plus the
// //go:build integration real-backend round-trip). What lives ONLY in cmd/gc — and
// is therefore what THIS test must lock — is candidate/route derivation order,
// legacy expansion, the run_target/routed_to route fan-out, and first-occurrence
// cross-query dedup. The fakeControlReadyQuerier models "what the store returns per
// sub-query" so the assertion isolates exactly that orchestration.
func TestControlReady_GoldenPipeline_DeriveThenFanOut(t *testing.T) {
	env := map[string]string{
		"GC_CONTROL_SESSION_NAME":  "rig/control-dispatcher",
		"GC_SESSION_NAME":          "rig/control-dispatcher", // dup of session-name candidate
		"GC_ALIAS":                 "rig-ctrl",               // no legacy expansion
		"GC_CONTROL_TARGET":        "rig/control-dispatcher",
		"GC_SESSION_ID":            "sess-xyz", // no legacy expansion
		"GC_CONTROL_LEGACY_TARGET": "rig/workflow-control",
	}
	candidates, routes := deriveControlReadyTargets(env)

	f := &fakeControlReadyQuerier{results: map[string][]beads.Bead{
		// Assignee sub-queries (one per distinct candidate; dup candidates re-run
		// the same query and must contribute nothing new).
		"assignee=rig/control-dispatcher": {{ID: "cd-1"}, {ID: "shared-cd-route"}},
		"assignee=rig/workflow-control":   {{ID: "wc-1"}},
		"assignee=rig-ctrl":               {{ID: "alias-1"}},
		"assignee=sess-xyz":               {{ID: "sess-1"}},
		// Route sub-queries: run_target + routed_to per route. shared-cd-route also
		// surfaces here and must dedup to its first (assignee) occurrence.
		"unassigned;gc.run_target=rig/control-dispatcher": {{ID: "shared-cd-route"}, {ID: "rt-1"}},
		"unassigned;gc.routed_to=rig/control-dispatcher":  {{ID: "rtd-1"}},
		"unassigned;gc.run_target=rig/workflow-control":   {{ID: "wc-rt-1"}},
		"unassigned;gc.routed_to=rig/workflow-control":    {{ID: "wc-rtd-1"}},
	}}

	got, err := controlDispatcherReadyBeads(f, candidates, routes, workflowServeScanLimit)
	if err != nil {
		t.Fatalf("controlDispatcherReadyBeads: %v", err)
	}

	want := []string{
		"cd-1", "shared-cd-route", // assignee=rig/control-dispatcher
		"wc-1",     // assignee=rig/workflow-control
		"alias-1",  // assignee=rig-ctrl
		"sess-1",   // assignee=sess-xyz
		"rt-1",     // route run_target=rig/control-dispatcher (shared-cd-route deduped)
		"rtd-1",    // route routed_to=rig/control-dispatcher
		"wc-rt-1",  // route run_target=rig/workflow-control
		"wc-rtd-1", // route routed_to=rig/workflow-control
	}
	if ids := hookIDs(got); !equalStrs(ids, want) {
		t.Fatalf("golden pipeline drift:\n got  %v\n want %v", ids, want)
	}
}

// controlDispatcherAgentCfgForTest returns a control-dispatcher config.Agent
// whose QualifiedName() ("rig/control-dispatcher") satisfies
// isWorkflowServeControlDispatcherAgent and whose empty WorkQuery satisfies the
// shell's control-ready gate (dispatch_runtime.go:326).
func controlDispatcherAgentCfgForTest(t *testing.T) config.Agent {
	t.Helper()
	return config.Agent{Name: config.ControlDispatcherAgentName, Dir: "rig"}
}

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
		controlDispatcherAgentCfgForTest(t),
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

// TestControlReadyServeSelection_FallsBackToShellWhenAgentHasCustomWorkQuery
// guards the parity gate (dispatch_runtime.go:326): the shell only substitutes
// the control-ready query when the control-dispatcher agent has NO custom
// WorkQuery. A control-dispatcher WITH a WorkQuery uses its expanded work query,
// so the in-process path MUST NOT hijack it — even though the store is capable.
func TestControlReadyServeSelection_FallsBackToShellWhenAgentHasCustomWorkQuery(t *testing.T) {
	prevOpener := controlReadyStoreOpener
	prevList := workflowServeList
	t.Cleanup(func() { controlReadyStoreOpener = prevOpener; workflowServeList = prevList })

	openerCalled := false
	controlReadyStoreOpener = func(storePath, cityPath string) (beads.ControlReadyQuerier, bool) {
		openerCalled = true
		return &fakeControlReadyQuerier{}, true // capable, but must not be used
	}
	shellCalled := false
	workflowServeList = func(workQuery, dir string, env map[string]string) ([]hookBead, error) {
		shellCalled = true
		return []hookBead{{ID: "shell-custom-1"}}, nil
	}

	agentCfg := controlDispatcherAgentCfgForTest(t)
	agentCfg.WorkQuery = "bd ready --assignee=rig/control-dispatcher --json --limit=1"

	got := serveControlReadyOrShell(
		agentCfg, "city", "store", "serveQuery",
		map[string]string{"GC_CONTROL_TARGET": "rig/control-dispatcher"},
		io.Discard,
	)
	if openerCalled {
		t.Fatalf("store opener must not run when the control-dispatcher has a custom WorkQuery")
	}
	if !shellCalled {
		t.Fatalf("shell path must run when the control-dispatcher has a custom WorkQuery")
	}
	if ids := hookIDs(got.queue); !equalStrs(ids, []string{"shell-custom-1"}) {
		t.Fatalf("custom-workquery fallback: got %v, want [shell-custom-1]", ids)
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
