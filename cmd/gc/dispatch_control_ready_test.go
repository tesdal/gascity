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
