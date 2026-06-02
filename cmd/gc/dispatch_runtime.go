package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/dispatch"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/shellquote"
	"github.com/gastownhall/gascity/internal/sling"
)

// graphExecutionRouteMetaKey is an alias for sling.GraphExecutionRouteMetaKey.
const graphExecutionRouteMetaKey = sling.GraphExecutionRouteMetaKey

// graphExecutionRigContextMetaKey is an alias for sling.GraphExecutionRigContextMetaKey.
const graphExecutionRigContextMetaKey = sling.GraphExecutionRigContextMetaKey

// isControlDispatcherKind delegates to sling.IsControlDispatcherKind.
func isControlDispatcherKind(kind string) bool {
	return sling.IsControlDispatcherKind(kind)
}

// workflowExecutionRoute delegates to sling.WorkflowExecutionRoute.
func workflowExecutionRoute(bead beads.Bead) string {
	return sling.WorkflowExecutionRoute(bead)
}

// controlDispatcherBinding delegates to sling.ControlDispatcherBinding.
func controlDispatcherBinding(store beads.Store, cityName string, cfg *config.City, rigContext string) (sling.GraphRouteBinding, error) {
	deps := sling.SlingDeps{
		CityName: cityName,
		Store:    store,
		Cfg:      cfg,
		Resolver: cliAgentResolver{},
	}
	return sling.ControlDispatcherBinding(store, cityName, cfg, rigContext, deps)
}

// assignGraphStepRoute delegates to sling.AssignGraphStepRoute.
func assignGraphStepRoute(step *formula.RecipeStep, executionBinding sling.GraphRouteBinding, controlBinding *sling.GraphRouteBinding) {
	sling.AssignGraphStepRoute(step, executionBinding, controlBinding)
}

// applyGraphRouting delegates to sling.ApplyGraphRouting with CLI interfaces.
func applyGraphRouting(recipe *formula.Recipe, a *config.Agent, routedTo string, vars map[string]string, scopeKind, scopeRef, storeRef string, store beads.Store, cityName, cityPath string, cfg *config.City) error {
	deps := sling.SlingDeps{
		CityName:              cityName,
		CityPath:              cityPath,
		Store:                 store,
		StoreRef:              storeRef,
		Cfg:                   cfg,
		Resolver:              cliAgentResolver{},
		DirectSessionResolver: cliDirectSessionResolver,
	}
	return sling.ApplyGraphRouting(recipe, a, routedTo, vars, "", scopeKind, scopeRef, storeRef, store, cityName, cfg, deps)
}

var (
	workflowServeList               = nextWorkflowServeBeads
	controlDispatcherServe          = runControlDispatcherInStore
	workflowServeOpenEventsProvider = func(stderr io.Writer) (events.Provider, error) {
		ep, code := openCityEventsProvider(stderr, "gc convoy control --serve")
		if ep == nil {
			return nil, fmt.Errorf("opening events provider (exit %d)", code)
		}
		return ep, nil
	}
	workflowServeIdlePollInterval  = 100 * time.Millisecond
	workflowServeIdlePollAttempts  = 3
	workflowServeWakeSweepInterval = 1 * time.Second
	workflowServeMaxIdleSleep      = 30 * time.Second
	workflowServeWaitForWake       = waitForRelevantWorkflowWakeWithTrace
	workflowTraceNow               = time.Now
	// The trace helper is intentionally process-global because workflowTracef
	// does not carry per-invocation context. Nested installs (serve ->
	// runControlDispatcherWithStore) reuse the active dedup map so one bad trace
	// path warns once per command invocation instead of once per control bead.
	// The newest installed scope owns the active writer; the most recent scope
	// for a given writer reuses that writer's dedupe map, and out-of-order
	// restores reactivate the newest remaining scope instead of panicking.
	// This assumes top-level callers are nested, not concurrently active from
	// separate goroutines in the same process.
	workflowTraceWarnings = struct {
		mu     sync.Mutex
		writer io.Writer
		warned map[string]struct{}
		scopes []workflowTraceWarningScope
		nextID uint64
	}{
		writer: os.Stderr,
		warned: map[string]struct{}{},
	}
)

// followSleepDuration returns the sleep interval the --follow loop should use
// before its next drain, given how many consecutive idle sweeps have passed.
// The idle sweep count doubles the base interval on each step, capped at
// workflowServeMaxIdleSleep. Fixes gastownhall/gascity#1028.
func followSleepDuration(idleSweeps int) time.Duration {
	if idleSweeps <= 0 {
		return workflowServeWakeSweepInterval
	}
	const maxShift = 30
	shift := idleSweeps
	if shift > maxShift {
		shift = maxShift
	}
	d := workflowServeWakeSweepInterval << uint(shift)
	if d <= 0 || d > workflowServeMaxIdleSleep {
		return workflowServeMaxIdleSleep
	}
	return d
}

const workflowServeScanLimit = 20

// runConvoyControlServe is the entry point for `gc convoy control --serve`.
func runConvoyControlServe(args []string, stdout, stderr io.Writer) error {
	var agentName string
	if len(args) > 0 {
		agentName = args[0]
	}
	if err := runWorkflowServe(agentName, true, stdout, stderr); err != nil {
		fmt.Fprintf(stderr, "gc convoy control --serve: %v\n", err) //nolint:errcheck
		return errExit
	}
	return nil
}

type hookBead struct {
	ID       string           `json:"id"`
	Metadata hookBeadMetadata `json:"metadata"`
}

type workflowTraceWarningScope struct {
	id     uint64
	writer io.Writer
	warned map[string]struct{}
}

// hookBeadMetadata handles metadata where values may be JSON strings,
// numbers, or booleans (bd writes numbers for numeric-looking values).
// Normalizes everything to strings on unmarshal.
type hookBeadMetadata map[string]string

func (m *hookBeadMetadata) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*m = make(hookBeadMetadata, len(raw))
	for k, v := range raw {
		var s string
		if json.Unmarshal(v, &s) == nil {
			(*m)[k] = s
		} else {
			// Non-string (number, bool): use raw JSON text without quotes.
			(*m)[k] = strings.Trim(string(v), " ")
		}
	}
	return nil
}

func workflowTracef(format string, args ...any) {
	path := strings.TrimSpace(os.Getenv("GC_WORKFLOW_TRACE"))
	if path == "" {
		path = strings.TrimSpace(os.Getenv("GC_SLING_TRACE"))
	}
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		workflowTraceWarnOpenFailure(path, err)
		return
	}
	defer f.Close()                                                                                            //nolint:errcheck // best-effort trace log
	fmt.Fprintf(f, "%s %s\n", workflowTraceNow().UTC().Format(time.RFC3339Nano), fmt.Sprintf(format, args...)) //nolint:errcheck
}

func workflowTraceWarnOpenFailure(path string, err error) {
	if strings.TrimSpace(path) == "" || err == nil {
		return
	}
	workflowTraceWarnings.mu.Lock()
	writer := workflowTraceWarnings.writer
	workflowTraceWarnings.mu.Unlock()
	workflowTraceWarnf(writer, "trace-open:"+normalizePathForCompare(path), "gc convoy control --serve: warning: opening workflow trace %q: %v\n", path, err)
}

func workflowTraceWarnf(writer io.Writer, dedupeKey, format string, args ...any) {
	if writer == nil {
		return
	}
	workflowTraceWarnings.mu.Lock()
	warned := workflowTraceWarnings.warned
	if workflowTraceWarnings.writer != writer || warned == nil {
		warned = nil
		for i := len(workflowTraceWarnings.scopes) - 1; i >= 0; i-- {
			if workflowTraceWarnings.scopes[i].writer == writer {
				warned = workflowTraceWarnings.scopes[i].warned
				break
			}
		}
	}
	if warned != nil {
		if _, alreadyWarned := warned[dedupeKey]; alreadyWarned {
			workflowTraceWarnings.mu.Unlock()
			return
		}
		warned[dedupeKey] = struct{}{}
	}
	workflowTraceWarnings.mu.Unlock()
	fmt.Fprintf(writer, format, args...) //nolint:errcheck // best-effort stderr
}

// useWorkflowTraceWarnings installs a per-command warning sink. Nested callers
// that share a writer reuse the same dedupe map so a single command invocation
// warns once per path. Restores may arrive out of order; the newest remaining
// scope stays active so helper reuse cannot panic the process.
func useWorkflowTraceWarnings(writer io.Writer) func() {
	workflowTraceWarnings.mu.Lock()
	workflowTraceWarnings.nextID++
	restoreID := workflowTraceWarnings.nextID
	warned := map[string]struct{}{}
	for i := len(workflowTraceWarnings.scopes) - 1; i >= 0; i-- {
		if workflowTraceWarnings.scopes[i].writer == writer {
			warned = workflowTraceWarnings.scopes[i].warned
			break
		}
	}
	workflowTraceWarnings.scopes = append(workflowTraceWarnings.scopes, workflowTraceWarningScope{
		id:     restoreID,
		writer: writer,
		warned: warned,
	})
	workflowTraceWarnings.writer = writer
	workflowTraceWarnings.warned = warned
	workflowTraceWarnings.mu.Unlock()
	return func() {
		workflowTraceWarnings.mu.Lock()
		defer workflowTraceWarnings.mu.Unlock()
		restoreIdx := -1
		for i := len(workflowTraceWarnings.scopes) - 1; i >= 0; i-- {
			if workflowTraceWarnings.scopes[i].id == restoreID {
				restoreIdx = i
				break
			}
		}
		if restoreIdx < 0 {
			return
		}
		workflowTraceWarnings.scopes = append(workflowTraceWarnings.scopes[:restoreIdx], workflowTraceWarnings.scopes[restoreIdx+1:]...)
		if n := len(workflowTraceWarnings.scopes); n > 0 {
			top := workflowTraceWarnings.scopes[n-1]
			workflowTraceWarnings.writer = top.writer
			workflowTraceWarnings.warned = top.warned
			return
		}
		workflowTraceWarnings.writer = os.Stderr
		workflowTraceWarnings.warned = map[string]struct{}{}
	}
}

func runWorkflowServe(agentName string, follow bool, _ io.Writer, stderr io.Writer) error {
	restoreTraceWarnings := useWorkflowTraceWarnings(stderr)
	defer restoreTraceWarnings()

	if follow {
		if err := requireWorkflowServeFollowSessionEnv(); err != nil {
			return err
		}
	}

	cityPath, err := resolveCity()
	if err != nil {
		return err
	}
	cfg, err := loadCityConfig(cityPath, stderr)
	if err != nil {
		return err
	}
	resolveRigPaths(cityPath, cfg.Rigs)
	warnLegacyWorkflowTracePath(cityPath, cfg.Rigs, stderr)
	if agentName == "" {
		agentName = os.Getenv("GC_ALIAS")
	}
	if agentName == "" {
		agentName = os.Getenv("GC_AGENT")
	}
	if agentName == "" || agentName == strings.TrimSpace(os.Getenv("GC_ALIAS")) || agentName == strings.TrimSpace(os.Getenv("GC_AGENT")) {
		template := strings.TrimSpace(os.Getenv("GC_TEMPLATE"))
		hasSessionContext := strings.TrimSpace(os.Getenv("GC_SESSION_NAME")) != "" ||
			strings.TrimSpace(os.Getenv("GC_SESSION_ID")) != ""
		if template != "" && hasSessionContext {
			agentName = template
		}
	}
	if agentName == "" {
		agentName = config.ControlDispatcherAgentName
	}
	agentCfg, ok := resolveAgentIdentity(cfg, agentName, currentRigContext(cfg))
	if !ok {
		return fmt.Errorf("agent %q not found in config", agentName)
	}
	workDir := agentCommandDir(cityPath, &agentCfg, cfg.Rigs)
	workEnv, err := controllerWorkQueryEnv(cityPath, cfg, &agentCfg)
	if err != nil {
		return fmt.Errorf("building work query env: %w", err)
	}
	cityName := loadedCityName(cfg, cityPath)
	// Expand {{.Rig}}/{{.AgentBase}} once so the long-poll drain reuses the
	// rig-scoped command instead of passing the literal template to the shell
	// on every iteration. #793.
	workQuery := expandAgentCommandTemplate(cityPath, cityName, &agentCfg, cfg.Rigs, "work_query", agentCfg.EffectiveWorkQuery(), stderr)
	if isWorkflowServeControlReadyAgent(agentCfg) {
		workQuery = applyControlReadyServeSetup(agentCfg, config.NamedSessionRuntimeName(cityName, cfg.Workspace, agentCfg.QualifiedName()), workEnv)
	}
	workflowTracef("serve start agent=%s city=%s dir=%s", agentCfg.QualifiedName(), cityPath, workDir)
	if !follow {
		_, err := drainWorkflowServeWork(agentCfg, cityPath, workDir, workQuery, workEnv, stderr)
		return err
	}
	return runWorkflowServeFollow(agentCfg, cityPath, workDir, workQuery, workEnv, stderr)
}

func requireWorkflowServeFollowSessionEnv() error {
	var missing []string
	for _, key := range []string{"GC_SESSION_ID", "GC_SESSION_NAME"} {
		if strings.TrimSpace(os.Getenv(key)) == "" {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("control dispatcher follow mode requires managed session env (%s not set)", strings.Join(missing, ", "))
	}
	return nil
}

func legacyWorkflowTracePaths(cityPath string, rigs []config.Rig) []string {
	paths := make([]string, 0, len(rigs)+1)
	seen := make(map[string]struct{}, len(rigs)+1)
	appendTracePath := func(root string) {
		root = strings.TrimSpace(root)
		if root == "" || !pathIsWithin(cityPath, root) {
			return
		}
		tracePath := filepath.Join(root, "control-dispatcher-trace.log")
		normalized := normalizePathForCompare(tracePath)
		if normalized == "" {
			return
		}
		if _, exists := seen[normalized]; exists {
			return
		}
		seen[normalized] = struct{}{}
		paths = append(paths, tracePath)
	}

	appendTracePath(cityPath)
	for _, rig := range rigs {
		appendTracePath(rig.Path)
	}
	appendTracePath(os.Getenv("GC_RIG_ROOT"))
	return paths
}

func warnLegacyWorkflowTracePath(cityPath string, rigs []config.Rig, stderr io.Writer) {
	if stderr == nil {
		return
	}
	legacyTracePaths := legacyWorkflowTracePaths(cityPath, rigs)
	nextTracePath := strings.TrimSpace(os.Getenv("GC_CONTROL_DISPATCHER_TRACE_DEFAULT"))
	if nextTracePath == "" {
		nextTracePath = citylayout.ControlDispatcherTraceDefaultPath(cityPath)
	}
	current := strings.TrimSpace(os.Getenv("GC_WORKFLOW_TRACE"))
	if current != "" {
		for _, legacyTracePath := range legacyTracePaths {
			if samePath(current, legacyTracePath) {
				workflowTraceWarnf(
					stderr,
					"legacy-trace-path:"+normalizePathForCompare(current),
					"gc convoy control --serve: warning: legacy control-dispatcher trace path %q matches a watcher-visible legacy location; change or unset GC_WORKFLOW_TRACE so this session adopts %q, or restart/recycle the session if this value was inherited before the upgrade\n",
					current,
					nextTracePath,
				)
				return
			}
		}
	}
	activeTracePath := current
	if activeTracePath == "" {
		activeTracePath = nextTracePath
	}
	for _, legacyTracePath := range legacyTracePaths {
		if samePath(activeTracePath, legacyTracePath) {
			continue
		}
		info, err := os.Stat(legacyTracePath)
		if err != nil || info.IsDir() {
			continue
		}
		workflowTraceWarnf(
			stderr,
			"legacy-trace-file:"+normalizePathForCompare(legacyTracePath),
			"gc convoy control --serve: warning: legacy control-dispatcher trace file %q still exists; writes to it can wake the city watcher. If it is still growing, restart or recycle the control-dispatcher session so it adopts %q.\n",
			legacyTracePath,
			nextTracePath,
		)
	}
}

type workflowServeDrainResult struct {
	processedAny bool
	pendingAny   bool
}

// drainWorkflowServeWork runs the control-dispatcher drain loop to completion
// for a single invocation. Returns whether it advanced a control bead and
// whether the queue still contains only pending work so the --follow caller
// can distinguish blocked work from genuine idle.
func drainWorkflowServeWork(agentCfg config.Agent, cityPath, storePath, workQuery string, workEnv map[string]string, stderr io.Writer) (workflowServeDrainResult, error) {
	result := workflowServeDrainResult{}
	idlePolls := 0
	for {
		serveQuery := workflowServeWorkQuery(agentCfg, workQuery)
		res := serveControlReadyOrShell(agentCfg, cityPath, storePath, serveQuery, workEnv, stderr)
		queue, err := res.queue, res.err
		if err != nil {
			workflowTracef("serve query-error agent=%s err=%v", agentCfg.QualifiedName(), err)
			// Surface a killed/timed-out control work query on the event
			// bus so the reconciler has a named cause to escalate on
			// rather than the session dying silently (issues #1496/#1497).
			emitCityWorkQueryFailure(cityPath, stderr,
				os.Getenv("GC_SESSION_ID"), os.Getenv("GC_TEMPLATE"), serveQuery, err)
			return result, fmt.Errorf("querying control work for %s: %w", agentCfg.QualifiedName(), err)
		}
		if len(queue) == 0 {
			if result.processedAny && idlePolls < workflowServeIdlePollAttempts {
				idlePolls++
				workflowTracef("serve idle-retry agent=%s attempt=%d", agentCfg.QualifiedName(), idlePolls)
				time.Sleep(workflowServeIdlePollInterval)
				continue
			}
			workflowTracef("serve idle-exit agent=%s", agentCfg.QualifiedName())
			return result, nil
		}
		idlePolls = 0
		processedThisCycle := false
		pendingCount := 0
		legacyOversizedCount := 0
		unexpectedKindCount := 0
		for _, candidate := range queue {
			beadID := candidate.ID
			kind := strings.TrimSpace(candidate.Metadata["gc.kind"])
			if !isControlDispatcherKind(kind) {
				unexpectedKindCount++
				workflowTracef("serve unexpected-kind-skip bead=%s kind=%s", beadID, kind)
				continue
			}
			workflowTracef("serve process bead=%s kind=%s store=%s", beadID, kind, storePath)
			// controlDispatcherServe currently returns nil both when it
			// successfully advanced a control bead AND when ProcessControl
			// chose to no-op (e.g., status != "open"). The caller cannot
			// tell those apart without cross-referencing the store, so the
			// trace line just below was previously identical in both
			// cases. That masked a 20-minute stall on ga-ttn5z's retry
			// control ga-fw2fm. The silent no-op now emits a separate
			// `process-control ... skip reason=bead_not_open` line inside
			// ProcessControl itself; see runtime.go.
			if err := controlDispatcherServe(cityPath, storePath, beadID, io.Discard, stderr); err != nil {
				if errors.Is(err, dispatch.ErrControlPending) {
					pendingCount++
					result.pendingAny = true
					workflowTracef("serve pending bead=%s kind=%s", beadID, kind)
					continue
				}
				workflowTracef("serve process-error bead=%s kind=%s err=%v", beadID, kind, err)
				if dispatch.IsTransientControllerError(err) {
					pendingCount++
					result.pendingAny = true
					workflowTracef("serve transient-error-pending bead=%s kind=%s err=%v", beadID, kind, err)
					continue
				}
				if isLegacyOversizedControlEventError(err) {
					legacyOversizedCount++
					continue
				}
				return result, fmt.Errorf("processing control bead %s: %w", beadID, err)
			}
			workflowTracef("serve processed bead=%s kind=%s", beadID, kind)
			result.processedAny = true
			processedThisCycle = true
		}
		if processedThisCycle {
			continue
		}
		if pendingCount > 0 {
			workflowTracef("serve pending-queue agent=%s count=%d", agentCfg.QualifiedName(), pendingCount)
			return result, nil
		}
		if legacyOversizedCount > 0 {
			workflowTracef("serve legacy-oversized-queue agent=%s count=%d", agentCfg.QualifiedName(), legacyOversizedCount)
			return result, nil
		}
		if unexpectedKindCount > 0 {
			workflowTracef("serve unexpected-kind-queue agent=%s count=%d", agentCfg.QualifiedName(), unexpectedKindCount)
			return result, nil
		}
	}
}

func isLegacyOversizedControlEventError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "recording attempt log") &&
		strings.Contains(msg, "old_value") &&
		strings.Contains(msg, "too large")
}

func runWorkflowServeFollow(agentCfg config.Agent, cityPath, storePath, workQuery string, workEnv map[string]string, stderr io.Writer) error {
	ep, err := workflowServeOpenEventsProvider(stderr)
	if err != nil {
		return err
	}
	defer ep.Close() //nolint:errcheck // best-effort cleanup

	afterSeq, err := ep.LatestSeq()
	if err != nil {
		return fmt.Errorf("reading current event cursor: %w", err)
	}
	watcher, err := ep.Watch(context.Background(), afterSeq)
	if err != nil {
		return fmt.Errorf("watching city events: %w", err)
	}
	defer watcher.Close() //nolint:errcheck // best-effort cleanup
	done := make(chan struct{})
	defer close(done)

	eventCh := make(chan workflowWatchResult, 1)
	go pumpWorkflowEvents(done, watcher, eventCh)

	idleSweeps := 0
	for {
		drainResult, err := drainWorkflowServeWork(agentCfg, cityPath, storePath, workQuery, workEnv, stderr)
		if err != nil {
			// A transient work-query/store failure — most commonly the
			// work-query timeout (hookWorkQueryTimeout) when the bead store is
			// briefly saturated — must NOT terminate this long-running serve
			// loop. drainWorkflowServeWork already surfaced the failure on the
			// event bus for reconciler visibility (#1496/#1497); returning here
			// kills the dispatcher process (pane exits non-zero) and leaves the
			// rig un-dispatched while its session bead still reports "active".
			// Downgrade to a no-progress sweep so the idle backoff retries it;
			// only genuinely fatal errors end the loop.
			if !dispatch.IsTransientControllerError(err) {
				return err
			}
			workflowTracef("serve drain-transient-retry agent=%s err=%v", agentCfg.QualifiedName(), err)
			drainResult = workflowServeDrainResult{}
		}
		if drainResult.processedAny || drainResult.pendingAny {
			idleSweeps = 0
		}
		sleepDur := followSleepDuration(idleSweeps)
		workflowTracef(
			"serve wait agent=%s idle_sweeps=%d sleep=%s processed=%t pending=%t",
			agentCfg.QualifiedName(),
			idleSweeps,
			sleepDur,
			drainResult.processedAny,
			drainResult.pendingAny,
		)
		eventWake, err := workflowServeWaitForWake(eventCh, sleepDur, idleSweeps)
		if err != nil {
			return err
		}
		switch {
		case eventWake, drainResult.pendingAny:
			idleSweeps = 0
		case !drainResult.processedAny:
			idleSweeps++
		}
	}
}

type workflowWatchResult struct {
	evt events.Event
	err error
}

func pumpWorkflowEvents(done <-chan struct{}, watcher events.Watcher, eventCh chan<- workflowWatchResult) {
	for {
		evt, err := watcher.Next()
		select {
		case eventCh <- workflowWatchResult{evt: evt, err: err}:
		case <-done:
			return
		}
		if err != nil {
			return
		}
	}
}

// waitForRelevantWorkflowWake blocks until either a relevant city event wakes
// the --follow loop or sleepDur elapses. Returns eventWake=true on the event
// path (so the caller can reset any idle-backoff counter), false when the
// timer fires.
func waitForRelevantWorkflowWake(eventCh <-chan workflowWatchResult, sleepDur time.Duration) (bool, error) {
	return waitForRelevantWorkflowWakeWithTrace(eventCh, sleepDur, -1)
}

func waitForRelevantWorkflowWakeWithTrace(eventCh <-chan workflowWatchResult, sleepDur time.Duration, idleSweeps int) (bool, error) {
	timer := time.NewTimer(sleepDur)
	defer timer.Stop()

	for {
		select {
		case res := <-eventCh:
			if res.err != nil {
				return false, res.err
			}
			if workflowEventRelevant(res.evt) {
				if idleSweeps >= 0 {
					workflowTracef("serve wake-event type=%s subject=%s idle_sweeps=%d sleep=%s", res.evt.Type, res.evt.Subject, idleSweeps, sleepDur)
				} else {
					workflowTracef("serve wake-event type=%s subject=%s", res.evt.Type, res.evt.Subject)
				}
				return true, nil
			}
			workflowTracef("serve ignore-event type=%s subject=%s", res.evt.Type, res.evt.Subject)
		case <-timer.C:
			if idleSweeps >= 0 {
				workflowTracef("serve wake-sweep idle_sweeps=%d sleep=%s", idleSweeps, sleepDur)
			} else {
				workflowTracef("serve wake-sweep")
			}
			return false, nil
		}
	}
}

func workflowEventRelevant(evt events.Event) bool {
	switch evt.Type {
	case events.BeadCreated, events.BeadClosed, events.BeadUpdated:
		return true
	default:
		return false
	}
}

func workflowServeQuery(workQuery string) string {
	const single = "--limit=1"
	scan := fmt.Sprintf("--limit=%d", workflowServeScanLimit)
	if strings.Contains(workQuery, single) {
		return strings.Replace(workQuery, single, scan, 1)
	}
	return workQuery
}

func workflowServeWorkQuery(agentCfg config.Agent, expandedWorkQuery ...string) string {
	if len(expandedWorkQuery) > 0 {
		return workflowServeQuery(expandedWorkQuery[0])
	}
	if isWorkflowServeControlReadyAgent(agentCfg) {
		return workflowServeControlReadyQuery(agentCfg)
	}
	workQuery := agentCfg.EffectiveWorkQuery()
	return workflowServeQuery(workQuery)
}

func isWorkflowServeControlDispatcherAgent(agentCfg config.Agent) bool {
	qualified := strings.TrimSpace(agentCfg.QualifiedName())
	return qualified == config.ControlDispatcherAgentName ||
		strings.HasSuffix(qualified, "/"+config.ControlDispatcherAgentName)
}

// applyControlReadyServeSetup configures the serve loop for a control-ready
// (control-dispatcher, no custom WorkQuery) agent. It returns the baked shell
// control-ready work query AND injects GC_CONTROL_SESSION_NAME into workEnv so the
// in-process path (controlReadyResolvedEnv -> deriveControlReadyTargets) derives
// the IDENTICAL candidate set the shell prefix bakes.
//
// Parity fix (codex review #1): the shell prefix ALWAYS sets GC_CONTROL_SESSION_NAME
// from config.NamedSessionRuntimeName(...). Previously the in-process path relied
// on GC_SESSION_NAME being present and equal to that value, which holds for managed
// --follow sessions but is not guaranteed in manual/non-follow contexts. Baking the
// exact same value into workEnv removes that implicit assumption so both paths see
// the same candidate ids. An empty session name is skipped (matching the shell
// prefix loop, which `continue`s on blank names).
func applyControlReadyServeSetup(agentCfg config.Agent, controlSessionName string, workEnv map[string]string) string {
	controlSessionName = strings.TrimSpace(controlSessionName)
	if controlSessionName != "" && workEnv != nil {
		workEnv["GC_CONTROL_SESSION_NAME"] = controlSessionName
	}
	return workflowServeControlReadyQuery(agentCfg, controlSessionName)
}

func workflowServeControlReadyQuery(agentCfg config.Agent, controlSessionNames ...string) string {
	target := strings.TrimSpace(agentCfg.QualifiedName())
	if target == "" {
		target = config.ControlDispatcherAgentName
	}
	limit := fmt.Sprintf("%d", workflowServeScanLimit)
	queryPrefix := `BD_EXPORT_AUTO=false GC_CONTROL_TARGET=` + shellquote.Quote(target)
	for _, name := range controlSessionNames {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		queryPrefix += ` GC_CONTROL_SESSION_NAME=` + shellquote.Quote(name)
		break
	}
	if legacy := workflowServeLegacyControlRoute(target); legacy != "" {
		queryPrefix += ` GC_CONTROL_LEGACY_TARGET=` + shellquote.Quote(legacy)
	}
	query := queryPrefix + ` sh -c '` +
		`tmp=$(mktemp); trap "rm -f \"$tmp\"" EXIT; ` +
		`emit_ready() { r=$("$@" 2>/dev/null || true); [ -n "$r" ] && [ "$r" != "[]" ] && printf "%s\n" "$r" >> "$tmp"; }; ` +
		`routed_ready() { route="$1"; [ -z "$route" ] && return 0; ` +
		`emit_ready bd --readonly --sandbox ready --include-ephemeral --metadata-field "gc.run_target=$route" --unassigned --exclude-type=epic --json --sort oldest --limit=` + limit + `; ` +
		`emit_ready bd --readonly --sandbox ready --include-ephemeral --metadata-field "gc.routed_to=$route" --unassigned --exclude-type=epic --json --sort oldest --limit=` + limit + `; ` +
		`}; ` +
		`for id in "$GC_CONTROL_SESSION_NAME" "$GC_SESSION_NAME" "$GC_ALIAS" "$GC_CONTROL_TARGET" "$GC_SESSION_ID"; do ` +
		`[ -z "$id" ] && continue; ` +
		`legacy=""; case "$id" in *control-dispatcher) legacy="${id%control-dispatcher}workflow-control";; esac; ` +
		`for cand in "$id" "$legacy"; do ` +
		`[ -z "$cand" ] && continue; ` +
		`emit_ready bd --readonly --sandbox ready --include-ephemeral --assignee="$cand" --exclude-type=epic --json --limit=` + limit + `; ` +
		`done; ` +
		`done; ` +
		`routed_ready "$GC_CONTROL_TARGET"; ` +
		`routed_ready "${GC_CONTROL_LEGACY_TARGET:-}"; ` +
		`[ -s "$tmp" ] && jq -s "reduce add[] as \$item ([]; if any(.[]; .id == \$item.id) then . else . + [\$item] end)" "$tmp" || printf "[]"` + `'`
	return query
}

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
// id followed by its candidate-loop legacy "workflow-control" variant; and routes
// as [GC_CONTROL_TARGET, GC_CONTROL_LEGACY_TARGET] (empties skipped). The legacy
// route is taken from env (as Task 5 bakes it, mirroring the shell prefix) and
// falls back to workflowServeLegacyControlRoute(target) — the same slash-aware
// helper the shell prefix uses — never the candidate-loop rule.
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
		// Route-legacy parity: the shell bakes GC_CONTROL_LEGACY_TARGET via
		// workflowServeLegacyControlRoute (slash-aware), which differs from the
		// candidate-loop rule (controlReadyLegacyCandidate) for non-"/"-delimited
		// ids. Prefer the env value Task 5 sets to match the shell prefix exactly;
		// derive with the SAME route helper only as a fallback so the in-process
		// path never diverges from the shell's routed_ready legacy route.
		legacy := strings.TrimSpace(env["GC_CONTROL_LEGACY_TARGET"])
		if legacy == "" {
			legacy = workflowServeLegacyControlRoute(target)
		}
		if legacy != "" {
			routes = append(routes, legacy)
		}
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
//
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

// controlReadyStoreOpener opens a store for the given paths and reports whether
// it can answer control-ready queries in-process. Swappable for tests. The
// production implementation reuses the same path gc status uses to select the
// store, so eligibility exactly matches reported beads_store.
//
// MAJOR review fix (store churn): the serve loop calls this on EVERY drain
// iteration (drainWorkflowServeWork inner loop). Opening a fresh native store
// each poll re-introduces per-cycle churn — the exact thing we are eliminating.
// The production opener therefore MEMOIZES by (storePath, cityPath): it opens
// once and reuses the handle for the lifetime of the serve process (which is
// bound to a single fixed scope). Tests swap the var with their own func, so
// memoization is invisible to them.
var controlReadyStoreOpener = newCachingControlReadyOpener(openControlReadyStore)

// openControlReadyStore is the production open func: it opens the store gc status
// would select and reports whether it can answer control-ready queries in-process.
func openControlReadyStore(storePath, cityPath string) (beads.ControlReadyQuerier, bool, error) {
	store, err := openStoreAtForCity(storePath, cityPath)
	if err != nil {
		return nil, false, err
	}
	q, ok := store.(beads.ControlReadyQuerier)
	return q, ok, nil
}

func newCachingControlReadyOpener(open func(storePath, cityPath string) (beads.ControlReadyQuerier, bool, error)) func(storePath, cityPath string) (beads.ControlReadyQuerier, bool) {
	var (
		mu        sync.Mutex
		cachedKey string
		cachedQ   beads.ControlReadyQuerier
		cachedOK  bool
		cached    bool
	)
	return func(storePath, cityPath string) (beads.ControlReadyQuerier, bool) {
		mu.Lock()
		defer mu.Unlock()
		key := storePath + "\x00" + cityPath
		if cached && key == cachedKey {
			return cachedQ, cachedOK
		}
		q, ok, err := open(storePath, cityPath)
		if err != nil {
			// Do NOT cache a transient open failure (codex review #4): caching
			// (nil,false) would permanently disable the native path for the
			// process lifetime on a one-time store hiccup. Falling back to the
			// shell this cycle is safe; the next poll retries the open. The
			// store-not-capable result (ok==false, err==nil) IS cached below
			// since that is a stable type fact, not a transient condition.
			return nil, false
		}
		cachedKey, cachedQ, cachedOK, cached = key, q, ok, true
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

// isWorkflowServeControlReadyAgent reports whether the serve loop substitutes the
// control-ready query for this agent. It mirrors the upstream gate in
// runWorkflowServe (dispatch_runtime.go:326): a control-dispatcher agent with NO
// custom WorkQuery. A control-dispatcher WITH a WorkQuery uses its expanded work
// query, so the in-process path must NOT hijack it. Both the upstream query
// builder and the serve seam reference this helper so the two stay in lockstep.
func isWorkflowServeControlReadyAgent(agentCfg config.Agent) bool {
	return agentCfg.WorkQuery == "" && isWorkflowServeControlDispatcherAgent(agentCfg)
}

// serveControlReadyOrShell returns the control-dispatcher ready queue, preferring
// the in-process capability and falling back to the shell query var otherwise.
func serveControlReadyOrShell(agentCfg config.Agent, cityPath, storePath, serveQuery string, workEnv map[string]string, stderr io.Writer) serveControlReadyResult {
	if isWorkflowServeControlReadyAgent(agentCfg) {
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
//
// GC_CONTROL_SESSION_NAME: the shell builder sets this from runWorkflowServe's
// config.NamedSessionRuntimeName(...) value. applyControlReadyServeSetup bakes the
// SAME value into workEnv at the gate, so it flows in here via mergeRuntimeEnv and
// deriveControlReadyTargets picks it up as the first candidate — matching the shell
// prefix exactly even when GC_SESSION_NAME is absent or differs (codex review #1).
func controlReadyResolvedEnv(agentCfg config.Agent, workEnv map[string]string) map[string]string {
	env := envSliceToMap(mergeRuntimeEnv(os.Environ(), workEnv))

	target := strings.TrimSpace(agentCfg.QualifiedName())
	if target == "" {
		target = config.ControlDispatcherAgentName
	}
	env["GC_CONTROL_TARGET"] = target

	if legacy := workflowServeLegacyControlRoute(target); legacy != "" {
		env["GC_CONTROL_LEGACY_TARGET"] = legacy
	}
	return env
}

func workflowServeLegacyControlRoute(target string) string {
	target = strings.TrimSpace(target)
	if target == config.ControlDispatcherAgentName {
		return "workflow-control"
	}
	const suffix = "/" + config.ControlDispatcherAgentName
	if strings.HasSuffix(target, suffix) {
		return strings.TrimSuffix(target, suffix) + "/workflow-control"
	}
	return ""
}

func nextWorkflowServeBeads(workQuery, dir string, env map[string]string) ([]hookBead, error) {
	if workQuery == "" {
		return nil, nil
	}
	output, err := shellWorkQueryWithEnv(workQuery, dir, mergeRuntimeEnv(os.Environ(), env))
	if err != nil {
		return nil, err
	}
	trimmed := strings.TrimSpace(output)
	if !workQueryHasReadyWork(trimmed) {
		return nil, nil
	}
	var beadsOut []hookBead
	if err := json.Unmarshal([]byte(trimmed), &beadsOut); err == nil {
		return beadsOut, nil
	}
	var bead hookBead
	if err := json.Unmarshal([]byte(trimmed), &bead); err == nil {
		return []hookBead{bead}, nil
	}
	return nil, fmt.Errorf("unexpected work query output: %s", trimmed)
}
