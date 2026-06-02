package main

import (
	"fmt"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
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
