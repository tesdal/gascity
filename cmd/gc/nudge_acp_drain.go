package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/telemetry"
	"github.com/gastownhall/gascity/internal/worker"
)

// drainACPQueuedNudges claims due queued nudges for ACP sessions and
// delivers them via the in-process session provider. It mirrors the
// poller path (tryDeliverQueuedNudgesByPoller) — session fencing,
// delivery blocking, message batching — so that nudge semantics are
// identical regardless of transport.
//
// This must run inside the supervisor/controller process where the ACP
// provider holds live connections.
//
// Returns the number of nudges successfully delivered.
func drainACPQueuedNudges(
	cityPath string,
	sp runtime.Provider,
	acpTargets []nudgeTarget,
	now time.Time,
) (int, error) {
	if len(acpTargets) == 0 {
		return 0, nil
	}

	totalDelivered := 0
	store := openNudgeBeadStore(cityPath)
	for _, target := range acpTargets {
		if !sp.IsRunning(target.sessionName) {
			continue
		}

		// Claim due nudges matching this target (agent key + session fence).
		claimed, err := claimDueQueuedNudgesForTarget(cityPath, target, now)
		if err != nil {
			return totalDelivered, fmt.Errorf("claiming ACP nudges for %s: %w", target.sessionName, err)
		}
		if len(claimed) == 0 {
			continue
		}

		// Filter out nudges that don't match the session fence
		// (SessionID / ContinuationEpoch mismatch).
		items, rejected := splitQueuedNudgesForTarget(target, claimed)
		if len(rejected) > 0 {
			if err := recordQueuedNudgeFailure(cityPath, queuedNudgeIDs(rejected), errNudgeSessionFenceMismatch, now); err != nil {
				return totalDelivered, fmt.Errorf("recording fenced ACP nudge failures: %w", err)
			}
		}

		// Filter out nudges blocked by wait-bead state (canceled,
		// expired, etc.).
		items, blocked, err := splitQueuedNudgesForDelivery(store, items)
		if err != nil {
			return totalDelivered, fmt.Errorf("checking ACP nudge delivery: %w", err)
		}
		if len(blocked) > 0 {
			if err := terminalizeBlockedQueuedNudges(cityPath, blocked); err != nil {
				return totalDelivered, fmt.Errorf("terminalizing blocked ACP nudges: %w", err)
			}
		}
		if len(items) == 0 {
			continue
		}

		// Batch into one message per session (matches poller behavior).
		msg := formatNudgeRuntimeMessage(items)

		// Deliver via worker handle to get delivery confirmation,
		// matching the poller path (tryDeliverQueuedNudgesByPoller).
		handle, err := workerHandleForNudgeTarget(target, store, sp)
		if err != nil {
			telemetry.RecordNudge(context.Background(), target.agentKey(), err)
			if recErr := recordQueuedNudgeFailure(cityPath, queuedNudgeIDs(items), err, now); recErr != nil {
				return totalDelivered, fmt.Errorf("recording ACP handle failure: %w", recErr)
			}
			continue
		}
		result, err := handle.Nudge(context.Background(), worker.NudgeRequest{
			Text:     msg,
			Delivery: worker.NudgeDeliveryDefault,
			Source:   "queue",
			Wake:     worker.NudgeWakeLiveOnly,
		})
		if err != nil {
			telemetry.RecordNudge(context.Background(), target.agentKey(), err)
			if recErr := recordQueuedNudgeFailure(cityPath, queuedNudgeIDs(items), err, now); recErr != nil {
				return totalDelivered, fmt.Errorf("recording ACP nudge failure: %w", recErr)
			}
			continue
		}
		if !result.Delivered {
			continue
		}

		telemetry.RecordNudge(context.Background(), target.agentKey(), nil)
		if err := ackQueuedNudges(cityPath, queuedNudgeIDs(items)); err != nil {
			return totalDelivered, fmt.Errorf("acking ACP nudges: %w", err)
		}
		totalDelivered += len(items)
	}

	return totalDelivered, nil
}

// buildACPNudgeTargets builds nudgeTarget values for all active ACP
// sessions, using session bead metadata for fencing (SessionID,
// ContinuationEpoch) and DesiredStateResult for ACP routing.
//
// This mirrors how resolveNudgeTargetFromSessionBead builds targets for
// the poller, but sources data from the reconciler's in-memory state
// instead of a fresh store query.
func buildACPNudgeTargets(
	cityPath string,
	cfg *config.City,
	result DesiredStateResult,
	sessionBeads *sessionBeadSnapshot,
) []nudgeTarget {
	// Build a set of ACP session names from desired state.
	acpSessions := make(map[string]TemplateParams)
	for _, tp := range result.State {
		if tp.IsACP && tp.SessionName != "" {
			acpSessions[tp.SessionName] = tp
		}
	}
	if len(acpSessions) == 0 {
		return nil
	}

	cityName := loadedCityName(cfg, cityPath)

	// Match session beads to ACP sessions for fencing metadata.
	var targets []nudgeTarget
	matched := make(map[string]bool)
	if sessionBeads != nil {
		for _, b := range sessionBeads.Open() {
			sessName := strings.TrimSpace(b.Metadata["session_name"])
			tp, ok := acpSessions[sessName]
			if !ok {
				continue
			}
			matched[sessName] = true
			targets = append(targets, nudgeTarget{
				cityPath:          cityPath,
				cityName:          cityName,
				cfg:               cfg,
				alias:             tp.Alias,
				identity:          firstNonEmpty(tp.InstanceName, tp.TemplateName),
				transport:         "acp",
				resolved:          tp.ResolvedProvider,
				sessionID:         b.ID,
				continuationEpoch: strings.TrimSpace(b.Metadata["continuation_epoch"]),
				sessionName:       sessName,
				aliasHistory:      session.AliasHistory(b.Metadata),
				agent:             resolveAgentForNudge(cfg, tp),
			})
		}
	}

	// ACP sessions without a session bead (e.g., just started, bead not
	// yet created) get a target without fencing — they'll only match
	// nudges that don't carry SessionID/ContinuationEpoch.
	for sessName, tp := range acpSessions {
		if matched[sessName] {
			continue
		}
		targets = append(targets, nudgeTarget{
			cityPath:    cityPath,
			cityName:    cityName,
			cfg:         cfg,
			alias:       tp.Alias,
			identity:    firstNonEmpty(tp.InstanceName, tp.TemplateName),
			transport:   "acp",
			resolved:    tp.ResolvedProvider,
			sessionName: sessName,
			agent:       resolveAgentForNudge(cfg, tp),
		})
	}

	return targets
}

// resolveAgentForNudge looks up the agent config for a TemplateParams.
func resolveAgentForNudge(cfg *config.City, tp TemplateParams) config.Agent {
	if cfg == nil {
		return config.Agent{}
	}
	for _, candidate := range []string{tp.InstanceName, tp.TemplateName, tp.Alias} {
		if candidate == "" {
			continue
		}
		found, ok := resolveAgentIdentity(cfg, candidate, "")
		if ok {
			return found
		}
	}
	return config.Agent{}
}
