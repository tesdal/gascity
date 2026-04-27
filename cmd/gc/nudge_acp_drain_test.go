package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/runtime"
)

func TestDrainACPQueuedNudges_DeliversDueNudge(t *testing.T) {
	cityPath := t.TempDir()
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "hermes--polecat", runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	now := time.Now()
	if err := nudgequeue.WithState(cityPath, func(s *nudgequeue.State) error {
		s.Pending = append(s.Pending, nudgequeue.Item{
			ID:           "nudge-1",
			Agent:        "hermes/polecat",
			Message:      "hello from queue",
			Source:       "session",
			CreatedAt:    now.Add(-1 * time.Second),
			DeliverAfter: now.Add(-1 * time.Second),
			ExpiresAt:    now.Add(24 * time.Hour),
		})
		return nil
	}); err != nil {
		t.Fatalf("WithState: %v", err)
	}

	targets := []nudgeTarget{{
		cityPath:    cityPath,
		alias:       "hermes/polecat",
		sessionName: "hermes--polecat",
		transport:   "acp",
	}}
	delivered, err := drainACPQueuedNudges(cityPath, sp, targets, now)
	if err != nil {
		t.Fatalf("drainACPQueuedNudges: %v", err)
	}
	if delivered != 1 {
		t.Errorf("delivered = %d, want 1", delivered)
	}

	// Verify exactly one Nudge call was made with the queued message.
	var nudgeCalled bool
	for _, c := range sp.Calls {
		if c.Method == "Nudge" && c.Name == "hermes--polecat" {
			nudgeCalled = true
			if !strings.Contains(c.Message, "hello from queue") {
				t.Errorf("Nudge message = %q, want to contain 'hello from queue'", c.Message)
			}
		}
	}
	if !nudgeCalled {
		t.Error("no Nudge call recorded — delivery did not happen")
	}

	// Verify nudge removed from both pending and in_flight (acked).
	var remaining nudgequeue.State
	if err := nudgequeue.WithState(cityPath, func(s *nudgequeue.State) error {
		remaining = *s
		return nil
	}); err != nil {
		t.Fatalf("WithState: %v", err)
	}
	if len(remaining.Pending) != 0 {
		t.Errorf("pending = %d, want 0", len(remaining.Pending))
	}
	if len(remaining.InFlight) != 0 {
		t.Errorf("in_flight = %d, want 0 (should be acked)", len(remaining.InFlight))
	}
}

func TestDrainACPQueuedNudges_SkipsNotYetDue(t *testing.T) {
	cityPath := t.TempDir()
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "hermes--polecat", runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	now := time.Now()
	if err := nudgequeue.WithState(cityPath, func(s *nudgequeue.State) error {
		s.Pending = append(s.Pending, nudgequeue.Item{
			ID:           "nudge-future",
			Agent:        "hermes/polecat",
			Message:      "not yet",
			Source:       "session",
			CreatedAt:    now,
			DeliverAfter: now.Add(1 * time.Hour),
			ExpiresAt:    now.Add(24 * time.Hour),
		})
		return nil
	}); err != nil {
		t.Fatalf("WithState: %v", err)
	}

	targets := []nudgeTarget{{
		cityPath:    cityPath,
		alias:       "hermes/polecat",
		sessionName: "hermes--polecat",
		transport:   "acp",
	}}
	delivered, err := drainACPQueuedNudges(cityPath, sp, targets, now)
	if err != nil {
		t.Fatalf("drainACPQueuedNudges: %v", err)
	}
	if delivered != 0 {
		t.Errorf("delivered = %d, want 0 (not yet due)", delivered)
	}

	// Nudge should still be pending — not claimed or delivered.
	var remaining nudgequeue.State
	if err := nudgequeue.WithState(cityPath, func(s *nudgequeue.State) error {
		remaining = *s
		return nil
	}); err != nil {
		t.Fatalf("WithState: %v", err)
	}
	if len(remaining.Pending) != 1 {
		t.Errorf("pending = %d, want 1 (not yet due)", len(remaining.Pending))
	}
	if len(remaining.InFlight) != 0 {
		t.Errorf("in_flight = %d, want 0", len(remaining.InFlight))
	}
	// No Nudge calls should have been made.
	for _, c := range sp.Calls {
		if c.Method == "Nudge" {
			t.Errorf("unexpected Nudge call for not-yet-due nudge: %v", c)
		}
	}
}

func TestDrainACPQueuedNudges_AgentMismatch_NotClaimed(t *testing.T) {
	cityPath := t.TempDir()
	sp := runtime.NewFake()
	// Start the polecat session so IsRunning passes — proving the skip
	// is due to agent-key mismatch in claim, not a session state issue.
	if err := sp.Start(context.Background(), "hermes--polecat", runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	now := time.Now()
	if err := nudgequeue.WithState(cityPath, func(s *nudgequeue.State) error {
		s.Pending = append(s.Pending, nudgequeue.Item{
			ID:           "nudge-dog",
			Agent:        "dog",
			Message:      "woof",
			Source:       "session",
			CreatedAt:    now.Add(-1 * time.Second),
			DeliverAfter: now.Add(-1 * time.Second),
			ExpiresAt:    now.Add(24 * time.Hour),
		})
		return nil
	}); err != nil {
		t.Fatalf("WithState: %v", err)
	}

	// Target is polecat; queued nudge is for "dog". The nudge should
	// not match because the agent keys differ.
	targets := []nudgeTarget{{
		cityPath:    cityPath,
		alias:       "hermes/polecat",
		sessionName: "hermes--polecat",
		transport:   "acp",
	}}
	delivered, err := drainACPQueuedNudges(cityPath, sp, targets, now)
	if err != nil {
		t.Fatalf("drainACPQueuedNudges: %v", err)
	}
	if delivered != 0 {
		t.Errorf("delivered = %d, want 0 (agent mismatch)", delivered)
	}

	// Nudge must remain pending — claim should not match it.
	var remaining nudgequeue.State
	if err := nudgequeue.WithState(cityPath, func(s *nudgequeue.State) error {
		remaining = *s
		return nil
	}); err != nil {
		t.Fatalf("WithState: %v", err)
	}
	if len(remaining.Pending) != 1 {
		t.Errorf("pending = %d, want 1 (dog nudge untouched)", len(remaining.Pending))
	}
	// No Nudge calls should have been made.
	for _, c := range sp.Calls {
		if c.Method == "Nudge" {
			t.Errorf("unexpected Nudge call: %v", c)
		}
	}
}

func TestDrainACPQueuedNudges_SessionNotRunning_Skipped(t *testing.T) {
	cityPath := t.TempDir()
	sp := runtime.NewFake()
	// Don't start the session — it won't be running.

	now := time.Now()
	if err := nudgequeue.WithState(cityPath, func(s *nudgequeue.State) error {
		s.Pending = append(s.Pending, nudgequeue.Item{
			ID:           "nudge-norun",
			Agent:        "hermes/polecat",
			Message:      "nobody home",
			Source:       "session",
			CreatedAt:    now.Add(-1 * time.Second),
			DeliverAfter: now.Add(-1 * time.Second),
			ExpiresAt:    now.Add(24 * time.Hour),
		})
		return nil
	}); err != nil {
		t.Fatalf("WithState: %v", err)
	}

	targets := []nudgeTarget{{
		cityPath:    cityPath,
		alias:       "hermes/polecat",
		sessionName: "hermes--polecat",
		transport:   "acp",
	}}
	delivered, err := drainACPQueuedNudges(cityPath, sp, targets, now)
	if err != nil {
		t.Fatalf("drainACPQueuedNudges: %v", err)
	}
	if delivered != 0 {
		t.Errorf("delivered = %d, want 0", delivered)
	}

	// Nudge should still be pending — session not running means we skip,
	// not claim-and-fail.
	var remaining nudgequeue.State
	if err := nudgequeue.WithState(cityPath, func(s *nudgequeue.State) error {
		remaining = *s
		return nil
	}); err != nil {
		t.Fatalf("WithState: %v", err)
	}
	if len(remaining.Pending) != 1 {
		t.Errorf("pending = %d, want 1 (left for next tick)", len(remaining.Pending))
	}
	// No Nudge calls — session not running means we skip entirely.
	for _, c := range sp.Calls {
		if c.Method == "Nudge" {
			t.Errorf("unexpected Nudge call for non-running session: %v", c)
		}
	}
}

func TestDrainACPQueuedNudges_SessionFenceMismatch_NotClaimed(t *testing.T) {
	cityPath := t.TempDir()
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "hermes--polecat", runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	now := time.Now()
	// Queue a nudge fenced to a different session ID.
	if err := nudgequeue.WithState(cityPath, func(s *nudgequeue.State) error {
		s.Pending = append(s.Pending, nudgequeue.Item{
			ID:           "nudge-fenced",
			Agent:        "hermes/polecat",
			SessionID:    "old-session-bead-id",
			Message:      "for old session",
			Source:       "session",
			CreatedAt:    now.Add(-1 * time.Second),
			DeliverAfter: now.Add(-1 * time.Second),
			ExpiresAt:    now.Add(24 * time.Hour),
		})
		return nil
	}); err != nil {
		t.Fatalf("WithState: %v", err)
	}

	// Target has a different session ID (current session). The fenced
	// nudge should not be claimable by this target.
	targets := []nudgeTarget{{
		cityPath:    cityPath,
		alias:       "hermes/polecat",
		sessionName: "hermes--polecat",
		sessionID:   "current-session-bead-id",
		transport:   "acp",
	}}
	delivered, err := drainACPQueuedNudges(cityPath, sp, targets, now)
	if err != nil {
		t.Fatalf("drainACPQueuedNudges: %v", err)
	}
	if delivered != 0 {
		t.Errorf("delivered = %d, want 0 (fenced nudge not claimable)", delivered)
	}

	// Nudge should remain in pending — it belongs to a different session.
	var remaining nudgequeue.State
	if err := nudgequeue.WithState(cityPath, func(s *nudgequeue.State) error {
		remaining = *s
		return nil
	}); err != nil {
		t.Fatalf("WithState: %v", err)
	}
	if len(remaining.Pending) != 1 {
		t.Errorf("pending = %d, want 1 (fenced nudge stays for its session)", len(remaining.Pending))
	}
	// No Nudge calls should have been made.
	for _, c := range sp.Calls {
		if c.Method == "Nudge" {
			t.Errorf("unexpected Nudge call: %v", c)
		}
	}
}

func TestDrainACPQueuedNudges_BatchesMultipleNudges(t *testing.T) {
	cityPath := t.TempDir()
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "hermes--polecat", runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	now := time.Now()
	if err := nudgequeue.WithState(cityPath, func(s *nudgequeue.State) error {
		for i, msg := range []string{"first", "second", "third"} {
			s.Pending = append(s.Pending, nudgequeue.Item{
				ID:           fmt.Sprintf("nudge-%d", i),
				Agent:        "hermes/polecat",
				Message:      msg,
				Source:       "session",
				CreatedAt:    now.Add(-1 * time.Second),
				DeliverAfter: now.Add(-1 * time.Second),
				ExpiresAt:    now.Add(24 * time.Hour),
			})
		}
		return nil
	}); err != nil {
		t.Fatalf("WithState: %v", err)
	}

	targets := []nudgeTarget{{
		cityPath:    cityPath,
		alias:       "hermes/polecat",
		sessionName: "hermes--polecat",
		transport:   "acp",
	}}
	delivered, err := drainACPQueuedNudges(cityPath, sp, targets, now)
	if err != nil {
		t.Fatalf("drainACPQueuedNudges: %v", err)
	}
	if delivered != 3 {
		t.Errorf("delivered = %d, want 3", delivered)
	}

	// Verify only ONE Nudge call was made (batched).
	nudgeCalls := 0
	var nudgeMsg string
	for _, c := range sp.Calls {
		if c.Method == "Nudge" && c.Name == "hermes--polecat" {
			nudgeCalls++
			nudgeMsg = c.Message
		}
	}
	if nudgeCalls != 1 {
		t.Errorf("Nudge calls = %d, want 1 (batched)", nudgeCalls)
	}
	// All three messages should appear in the batched output.
	for _, msg := range []string{"first", "second", "third"} {
		if !strings.Contains(nudgeMsg, msg) {
			t.Errorf("batched message missing %q: %s", msg, nudgeMsg)
		}
	}
}

func TestDrainACPQueuedNudges_NoTargets(t *testing.T) {
	cityPath := t.TempDir()
	sp := runtime.NewFake()

	now := time.Now()
	if err := nudgequeue.WithState(cityPath, func(s *nudgequeue.State) error {
		s.Pending = append(s.Pending, nudgequeue.Item{
			ID:           "nudge-orphan",
			Agent:        "hermes/polecat",
			Message:      "should stay",
			Source:       "session",
			CreatedAt:    now.Add(-1 * time.Second),
			DeliverAfter: now.Add(-1 * time.Second),
			ExpiresAt:    now.Add(24 * time.Hour),
		})
		return nil
	}); err != nil {
		t.Fatalf("WithState: %v", err)
	}

	delivered, err := drainACPQueuedNudges(cityPath, sp, nil, now)
	if err != nil {
		t.Fatalf("drainACPQueuedNudges: %v", err)
	}
	if delivered != 0 {
		t.Errorf("delivered = %d, want 0", delivered)
	}

	// Queue should be untouched.
	var remaining nudgequeue.State
	if err := nudgequeue.WithState(cityPath, func(s *nudgequeue.State) error {
		remaining = *s
		return nil
	}); err != nil {
		t.Fatalf("WithState: %v", err)
	}
	if len(remaining.Pending) != 1 {
		t.Errorf("pending = %d, want 1 (untouched)", len(remaining.Pending))
	}
	for _, c := range sp.Calls {
		if c.Method == "Nudge" {
			t.Errorf("unexpected Nudge call: %v", c)
		}
	}
}

func TestBuildACPNudgeTargets(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{Agents: []config.Agent{{Name: "polecat", Dir: "hermes"}}}

	// Create a session bead with fencing metadata.
	store := beads.NewMemStore()
	sessionBead, err := store.Create(beads.Bead{
		Title:  "polecat session",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":       "hermes--polecat",
			"continuation_epoch": "3",
			"agent_name":         "hermes/polecat",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	snapshot := newSessionBeadSnapshot([]beads.Bead{sessionBead})

	result := DesiredStateResult{
		State: map[string]TemplateParams{
			"hermes--polecat": {SessionName: "hermes--polecat", IsACP: true, Alias: "hermes/polecat", TemplateName: "polecat"},
			"dog-1":           {SessionName: "dog-1", IsACP: false, Alias: "dog"},
		},
	}

	targets := buildACPNudgeTargets(cityPath, cfg, result, snapshot)
	if len(targets) != 1 {
		t.Fatalf("targets = %d, want 1", len(targets))
	}
	target := targets[0]
	if target.sessionName != "hermes--polecat" {
		t.Errorf("sessionName = %q, want hermes--polecat", target.sessionName)
	}
	if target.alias != "hermes/polecat" {
		t.Errorf("alias = %q, want hermes/polecat", target.alias)
	}
	if target.sessionID != sessionBead.ID {
		t.Errorf("sessionID = %q, want %q (from session bead)", target.sessionID, sessionBead.ID)
	}
	if target.continuationEpoch != "3" {
		t.Errorf("continuationEpoch = %q, want '3'", target.continuationEpoch)
	}
	if target.transport != "acp" {
		t.Errorf("transport = %q, want 'acp'", target.transport)
	}
}

func TestBuildACPNudgeTargets_NoSessionBead(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{Agents: []config.Agent{{Name: "polecat", Dir: "hermes"}}}

	result := DesiredStateResult{
		State: map[string]TemplateParams{
			"hermes--polecat": {SessionName: "hermes--polecat", IsACP: true, Alias: "hermes/polecat"},
		},
	}

	// No session beads — target should still be created, but without fencing.
	targets := buildACPNudgeTargets(cityPath, cfg, result, newSessionBeadSnapshot(nil))
	if len(targets) != 1 {
		t.Fatalf("targets = %d, want 1", len(targets))
	}
	if targets[0].sessionID != "" {
		t.Errorf("sessionID = %q, want empty (no bead)", targets[0].sessionID)
	}
	if targets[0].continuationEpoch != "" {
		t.Errorf("continuationEpoch = %q, want empty (no bead)", targets[0].continuationEpoch)
	}
	if targets[0].sessionName != "hermes--polecat" {
		t.Errorf("sessionName = %q, want hermes--polecat", targets[0].sessionName)
	}
	if targets[0].alias != "hermes/polecat" {
		t.Errorf("alias = %q, want hermes/polecat", targets[0].alias)
	}
	if targets[0].transport != "acp" {
		t.Errorf("transport = %q, want acp", targets[0].transport)
	}
}

func TestBeadReconcileTick_DrainsACPQueuedNudges(t *testing.T) {
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "hermes--polecat", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	cityPath := t.TempDir()

	now := time.Now()
	if err := nudgequeue.WithState(cityPath, func(s *nudgequeue.State) error {
		s.Pending = append(s.Pending, nudgequeue.Item{
			ID:           "nudge-via-tick",
			Agent:        "hermes/polecat",
			Message:      "tick nudge",
			Source:       "session",
			CreatedAt:    now.Add(-1 * time.Second),
			DeliverAfter: now.Add(-1 * time.Second),
			ExpiresAt:    now.Add(24 * time.Hour),
		})
		return nil
	}); err != nil {
		t.Fatalf("WithState: %v", err)
	}

	cr := &CityRuntime{
		cityPath:            cityPath,
		cityName:            "test-city",
		cfg:                 &config.City{Agents: []config.Agent{{Name: "polecat", Dir: "hermes"}}},
		sp:                  sp,
		standaloneCityStore: beads.NewMemStore(),
		sessionDrains:       newDrainTracker(),
		rec:                 events.Discard,
		stdout:              io.Discard,
		stderr:              io.Discard,
	}

	result := DesiredStateResult{
		State: map[string]TemplateParams{
			"hermes--polecat": {SessionName: "hermes--polecat", IsACP: true, Alias: "hermes/polecat"},
		},
	}

	sessionBeads := newSessionBeadSnapshot(nil)
	cr.beadReconcileTick(context.Background(), result, sessionBeads, nil)

	// Verify the nudge was delivered via Nudge call.
	var found bool
	for _, c := range sp.Calls {
		if c.Method == "Nudge" && c.Name == "hermes--polecat" && strings.Contains(c.Message, "tick nudge") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected Nudge call containing 'tick nudge', got calls: %v", sp.Calls)
	}

	// Verify the queue is empty (both pending and in_flight).
	var remaining nudgequeue.State
	if err := nudgequeue.WithState(cityPath, func(s *nudgequeue.State) error {
		remaining = *s
		return nil
	}); err != nil {
		t.Fatalf("WithState: %v", err)
	}
	if len(remaining.Pending) != 0 {
		t.Errorf("pending = %d, want 0", len(remaining.Pending))
	}
	if len(remaining.InFlight) != 0 {
		t.Errorf("in_flight = %d, want 0", len(remaining.InFlight))
	}
}

// stoppingProvider wraps a Fake and stops the named session after the
// first IsRunning call returns true. This simulates a session stopping
// between the drain's IsRunning pre-check and the worker handle's
// delivery attempt, ensuring nudges are not acked without confirmation.
type stoppingProvider struct {
	*runtime.Fake
	stopAfter string
	stopped   bool
}

func (p *stoppingProvider) IsRunning(name string) bool {
	running := p.Fake.IsRunning(name)
	if running && name == p.stopAfter && !p.stopped {
		p.stopped = true
		_ = p.Stop(name)
	}
	return running
}

// nudgeErrProvider wraps a Fake so that Nudge returns an error while
// IsRunning still reports true. This exercises the handle.Nudge error
// path (telemetry recording + failure recording) without affecting the
// running state pre-check.
type nudgeErrProvider struct {
	*runtime.Fake
	nudgeErr error
}

func (p *nudgeErrProvider) Nudge(name string, content []runtime.ContentBlock) error {
	p.Fake.Nudge(name, content) //nolint:errcheck // record the call
	return p.nudgeErr
}

func TestDrainACPQueuedNudges_SessionStopsBeforeDelivery_NotAcked(t *testing.T) {
	cityPath := t.TempDir()
	fake := runtime.NewFake()
	if err := fake.Start(context.Background(), "hermes--polecat", runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Wrap: first IsRunning returns true (drain pre-check passes),
	// but stops the session so the worker handle's IsRunning returns false.
	sp := &stoppingProvider{Fake: fake, stopAfter: "hermes--polecat"}

	now := time.Now()
	if err := nudgequeue.WithState(cityPath, func(s *nudgequeue.State) error {
		s.Pending = append(s.Pending, nudgequeue.Item{
			ID:           "nudge-vanish",
			Agent:        "hermes/polecat",
			Message:      "should not be acked",
			Source:       "session",
			CreatedAt:    now.Add(-1 * time.Second),
			DeliverAfter: now.Add(-1 * time.Second),
			ExpiresAt:    now.Add(24 * time.Hour),
		})
		return nil
	}); err != nil {
		t.Fatalf("WithState: %v", err)
	}

	targets := []nudgeTarget{{
		cityPath:    cityPath,
		alias:       "hermes/polecat",
		sessionName: "hermes--polecat",
		transport:   "acp",
	}}
	delivered, err := drainACPQueuedNudges(cityPath, sp, targets, now)
	if err != nil {
		t.Fatalf("drainACPQueuedNudges: %v", err)
	}
	if !sp.stopped {
		t.Error("stoppingProvider did not trigger — race path was not exercised")
	}
	if delivered != 0 {
		t.Errorf("delivered = %d, want 0 (session stopped before delivery)", delivered)
	}

	// Nudge must NOT be acked — it should remain in-flight (claimed but
	// not delivered) so a future tick can retry.
	var remaining nudgequeue.State
	if err := nudgequeue.WithState(cityPath, func(s *nudgequeue.State) error {
		remaining = *s
		return nil
	}); err != nil {
		t.Fatalf("WithState: %v", err)
	}
	if len(remaining.InFlight) != 1 {
		t.Errorf("in_flight = %d, want 1 (claimed but not delivered)", len(remaining.InFlight))
	}
	if len(remaining.Pending) != 0 {
		t.Errorf("pending = %d, want 0 (should have been claimed)", len(remaining.Pending))
	}
}

func TestDrainACPQueuedNudges_NudgeError_RecordsFailure(t *testing.T) {
	cityPath := t.TempDir()
	fake := runtime.NewFake()
	if err := fake.Start(context.Background(), "hermes--polecat", runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	sp := &nudgeErrProvider{Fake: fake, nudgeErr: fmt.Errorf("connection reset")}

	now := time.Now()
	if err := nudgequeue.WithState(cityPath, func(s *nudgequeue.State) error {
		s.Pending = append(s.Pending, nudgequeue.Item{
			ID:           "nudge-err",
			Agent:        "hermes/polecat",
			Message:      "will fail",
			Source:       "session",
			CreatedAt:    now.Add(-1 * time.Second),
			DeliverAfter: now.Add(-1 * time.Second),
			ExpiresAt:    now.Add(24 * time.Hour),
		})
		return nil
	}); err != nil {
		t.Fatalf("WithState: %v", err)
	}

	targets := []nudgeTarget{{
		cityPath:    cityPath,
		alias:       "hermes/polecat",
		sessionName: "hermes--polecat",
		transport:   "acp",
	}}
	delivered, err := drainACPQueuedNudges(cityPath, sp, targets, now)
	if err != nil {
		t.Fatalf("drainACPQueuedNudges: %v", err)
	}
	if delivered != 0 {
		t.Errorf("delivered = %d, want 0 (nudge errored)", delivered)
	}

	// Verify the Nudge call was actually attempted (proves the error
	// comes from handle.Nudge, not an earlier short-circuit).
	var nudgeAttempted bool
	for _, c := range fake.Calls {
		if c.Method == "Nudge" && c.Name == "hermes--polecat" {
			nudgeAttempted = true
			break
		}
	}
	if !nudgeAttempted {
		t.Error("no Nudge call recorded — error path was not reached")
	}

	// Nudge should be requeued after failure (not acked, not stuck in-flight).
	var remaining nudgequeue.State
	if err := nudgequeue.WithState(cityPath, func(s *nudgequeue.State) error {
		remaining = *s
		return nil
	}); err != nil {
		t.Fatalf("WithState: %v", err)
	}
	if len(remaining.Pending) != 1 {
		t.Errorf("pending = %d, want 1 (requeued after failure)", len(remaining.Pending))
	}
	if len(remaining.InFlight) != 0 {
		t.Errorf("in_flight = %d, want 0 (not stuck in-flight)", len(remaining.InFlight))
	}
	if remaining.Pending[0].Attempts != 1 {
		t.Errorf("attempts = %d, want 1 (single failure recorded)", remaining.Pending[0].Attempts)
	}
	if remaining.Pending[0].LastError == "" {
		t.Error("last_error is empty, want error message recorded")
	}
}

func TestDrainACPQueuedNudges_StaleEpoch_ClaimedThenRejected(t *testing.T) {
	cityPath := t.TempDir()
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "hermes--polecat", runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	now := time.Now()
	// Queue a nudge fenced to the same session ID but a stale epoch.
	// claimDueQueuedNudgesForTarget will claim it (session ID matches),
	// but splitQueuedNudgesForTarget will reject it (epoch mismatch).
	if err := nudgequeue.WithState(cityPath, func(s *nudgequeue.State) error {
		s.Pending = append(s.Pending, nudgequeue.Item{
			ID:                "nudge-stale-epoch",
			Agent:             "hermes/polecat",
			SessionID:         "sess-bead-1",
			ContinuationEpoch: "2",
			Message:           "for old epoch",
			Source:            "session",
			CreatedAt:         now.Add(-1 * time.Second),
			DeliverAfter:      now.Add(-1 * time.Second),
			ExpiresAt:         now.Add(24 * time.Hour),
		})
		return nil
	}); err != nil {
		t.Fatalf("WithState: %v", err)
	}

	// Target has the same session ID but a newer epoch.
	targets := []nudgeTarget{{
		cityPath:          cityPath,
		alias:             "hermes/polecat",
		sessionName:       "hermes--polecat",
		sessionID:         "sess-bead-1",
		continuationEpoch: "3",
		transport:         "acp",
	}}
	delivered, err := drainACPQueuedNudges(cityPath, sp, targets, now)
	if err != nil {
		t.Fatalf("drainACPQueuedNudges: %v", err)
	}
	if delivered != 0 {
		t.Errorf("delivered = %d, want 0 (stale epoch rejected)", delivered)
	}

	// The nudge was claimed (removed from pending) then rejected by the
	// fence split, so it should be recorded as failed — not left pending
	// or in-flight.
	var remaining nudgequeue.State
	if err := nudgequeue.WithState(cityPath, func(s *nudgequeue.State) error {
		remaining = *s
		return nil
	}); err != nil {
		t.Fatalf("WithState: %v", err)
	}
	if len(remaining.Pending) != 0 {
		t.Errorf("pending = %d, want 0 (claimed and failed)", len(remaining.Pending))
	}
	if len(remaining.InFlight) != 0 {
		t.Errorf("in_flight = %d, want 0 (fence rejection recorded)", len(remaining.InFlight))
	}
	// No Nudge calls — rejected before delivery attempt.
	for _, c := range sp.Calls {
		if c.Method == "Nudge" {
			t.Errorf("unexpected Nudge call for stale-epoch nudge: %v", c)
		}
	}
}

func TestDrainACPQueuedNudges_MultipleTargets_MixedResults(t *testing.T) {
	cityPath := t.TempDir()
	fake := runtime.NewFake()
	// Start polecat (will succeed) but not owl (will be skipped).
	if err := fake.Start(context.Background(), "hermes--polecat", runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("Start polecat: %v", err)
	}

	now := time.Now()
	if err := nudgequeue.WithState(cityPath, func(s *nudgequeue.State) error {
		s.Pending = append(s.Pending, nudgequeue.Item{
			ID:           "nudge-polecat",
			Agent:        "hermes/polecat",
			Message:      "polecat msg",
			Source:       "session",
			CreatedAt:    now.Add(-1 * time.Second),
			DeliverAfter: now.Add(-1 * time.Second),
			ExpiresAt:    now.Add(24 * time.Hour),
		}, nudgequeue.Item{
			ID:           "nudge-owl",
			Agent:        "hermes/owl",
			Message:      "owl msg",
			Source:       "session",
			CreatedAt:    now.Add(-1 * time.Second),
			DeliverAfter: now.Add(-1 * time.Second),
			ExpiresAt:    now.Add(24 * time.Hour),
		})
		return nil
	}); err != nil {
		t.Fatalf("WithState: %v", err)
	}

	targets := []nudgeTarget{
		{
			cityPath:    cityPath,
			alias:       "hermes/polecat",
			sessionName: "hermes--polecat",
			transport:   "acp",
		},
		{
			cityPath:    cityPath,
			alias:       "hermes/owl",
			sessionName: "hermes--owl",
			transport:   "acp",
		},
	}
	delivered, err := drainACPQueuedNudges(cityPath, fake, targets, now)
	if err != nil {
		t.Fatalf("drainACPQueuedNudges: %v", err)
	}
	// Only polecat should deliver (owl is not running).
	if delivered != 1 {
		t.Errorf("delivered = %d, want 1 (only polecat running)", delivered)
	}

	// Verify polecat nudge acked, owl nudge still pending.
	var remaining nudgequeue.State
	if err := nudgequeue.WithState(cityPath, func(s *nudgequeue.State) error {
		remaining = *s
		return nil
	}); err != nil {
		t.Fatalf("WithState: %v", err)
	}
	if len(remaining.Pending) != 1 {
		t.Fatalf("pending = %d, want 1 (owl nudge remains)", len(remaining.Pending))
	}
	if remaining.Pending[0].ID != "nudge-owl" {
		t.Errorf("remaining pending ID = %q, want nudge-owl", remaining.Pending[0].ID)
	}
	if len(remaining.InFlight) != 0 {
		t.Errorf("in_flight = %d, want 0", len(remaining.InFlight))
	}

	// Verify Nudge was only called for polecat.
	for _, c := range fake.Calls {
		if c.Method == "Nudge" && c.Name == "hermes--owl" {
			t.Errorf("unexpected Nudge call for non-running owl session")
		}
	}
}

func TestResolveAgentForNudge_CandidatePriority(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{
		{Name: "polecat", Dir: "hermes"},
		{Name: "owl", Dir: "barn"},
	}}

	// InstanceName takes priority over TemplateName.
	got := resolveAgentForNudge(cfg, TemplateParams{
		InstanceName: "hermes/polecat",
		TemplateName: "owl",
		Alias:        "barn/owl",
	})
	if got.Name != "polecat" {
		t.Errorf("Name = %q, want polecat (InstanceName priority)", got.Name)
	}

	// Falls through to TemplateName when InstanceName is empty.
	got = resolveAgentForNudge(cfg, TemplateParams{
		TemplateName: "owl",
		Alias:        "hermes/polecat",
	})
	if got.Name != "owl" {
		t.Errorf("Name = %q, want owl (TemplateName fallback)", got.Name)
	}

	// Falls through to Alias when InstanceName and TemplateName don't match.
	got = resolveAgentForNudge(cfg, TemplateParams{
		InstanceName: "nonexistent/thing",
		TemplateName: "also-nonexistent",
		Alias:        "hermes/polecat",
	})
	if got.Name != "polecat" {
		t.Errorf("Name = %q, want polecat (Alias fallback)", got.Name)
	}

	// Returns empty when nothing matches.
	got = resolveAgentForNudge(cfg, TemplateParams{
		Alias: "nonexistent/agent",
	})
	if got.Name != "" {
		t.Errorf("Name = %q, want empty (no match)", got.Name)
	}

	// nil config returns empty.
	got = resolveAgentForNudge(nil, TemplateParams{TemplateName: "polecat"})
	if got.Name != "" {
		t.Errorf("Name = %q, want empty (nil config)", got.Name)
	}
}
