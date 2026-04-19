package main

// cityinit.Initializer implementation. Bridges the domain interface
// declared in internal/cityinit to the concrete scaffold + finalize
// helpers in this package. Supplied to api.NewSupervisorMux at
// construction so POST /v0/city calls Init in-process — no
// subprocess, no 30-second deadline, no stderr-scraping.
//
// The long-term plan is to move doInit/finalizeInit and their
// helpers into internal/cityinit so the domain logic physically
// lives in the object model (per specs/architecture.md §1). This
// bridge is the minimum viable unblocker: the HTTP API no longer
// shells out, both surfaces drive the same in-process code path via
// the same typed contract, and the refactor of where the body lives
// is a follow-up.

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/cityinit"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// localInitializer implements cityinit.Initializer by dispatching to
// this package's existing doInit + finalizeInit functions.
type localInitializer struct{}

// NewInitializer returns the Initializer the supervisor uses to
// service POST /v0/city. Exported so `gc supervisor run` can wire it
// into api.NewSupervisorMux.
func NewInitializer() cityinit.Initializer {
	return localInitializer{}
}

// Scaffold runs the fast portion of city creation so the HTTP API
// handler can return 202 Accepted without blocking on the slow
// finalize work. Writes the on-disk shape (via doInit), then
// registers the city with the supervisor so the reconciler picks
// it up on its next tick. The reconciler owns finalize from there;
// readiness is signaled via city.ready / city.init_failed events on
// the supervisor event bus (see internal/api/event_payloads.go).
func (localInitializer) Scaffold(_ context.Context, req cityinit.InitRequest) (*cityinit.InitResult, error) {
	if err := validateInitRequest(&req); err != nil {
		return nil, err
	}
	dir := req.Dir
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating directory %q: %w", dir, err)
	}

	wiz := wizardConfig{
		configName:       req.ConfigName,
		provider:         req.Provider,
		startCommand:     req.StartCommand,
		bootstrapProfile: req.BootstrapProfile,
	}
	if wiz.configName == "" {
		wiz.configName = "tutorial"
	}

	if cityHasScaffoldFS(fsys.OSFS{}, dir) {
		return nil, cityinit.ErrAlreadyInitialized
	}
	if code := doInit(fsys.OSFS{}, dir, wiz, req.NameOverride, io.Discard, io.Discard); code != 0 {
		if code == initExitAlreadyInitialized {
			return nil, cityinit.ErrAlreadyInitialized
		}
		return nil, fmt.Errorf("scaffold failed (exit %d)", code)
	}

	// Register the city with the supervisor so the reconciler picks
	// it up on its next tick. API-created cities land in the
	// registry; prepareCityForSupervisor runs asynchronously and
	// emits city.ready / city.init_failed when done.
	if code := registerCityWithSupervisor(dir, io.Discard, io.Discard, "POST /v0/city", false); code != 0 {
		return nil, fmt.Errorf("register with supervisor failed (exit %d)", code)
	}

	cityName := resolveCityName(req.NameOverride, dir)
	return &cityinit.InitResult{
		CityName:     cityName,
		CityPath:     dir,
		ProviderUsed: req.Provider,
	}, nil
}

// Init scaffolds + finalizes a new city. Errors are mapped to the
// typed sentinels in package cityinit so callers (HTTP API, future
// in-process consumers) can pattern-match via errors.Is.
func (localInitializer) Init(_ context.Context, req cityinit.InitRequest) (*cityinit.InitResult, error) {
	if err := validateInitRequest(&req); err != nil {
		return nil, err
	}
	dir := req.Dir
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating directory %q: %w", dir, err)
	}

	wiz := wizardConfig{
		configName:       req.ConfigName,
		provider:         req.Provider,
		startCommand:     req.StartCommand,
		bootstrapProfile: req.BootstrapProfile,
	}
	if wiz.configName == "" {
		wiz.configName = "tutorial"
	}

	// Check for an already-initialized directory BEFORE calling
	// doInit so we can return ErrAlreadyInitialized without also
	// writing "gc init: already initialized" to stderr (the CLI
	// path wants that; the API does not).
	if cityHasScaffoldFS(fsys.OSFS{}, dir) {
		return nil, cityinit.ErrAlreadyInitialized
	}

	// doInit writes directly to io.Writer arguments (CLI progress
	// narration). The API path discards those; error return is
	// carried as an exit code, which we translate into typed
	// sentinels below.
	if code := doInit(fsys.OSFS{}, dir, wiz, req.NameOverride, io.Discard, io.Discard); code != 0 {
		if code == initExitAlreadyInitialized {
			return nil, cityinit.ErrAlreadyInitialized
		}
		return nil, fmt.Errorf("scaffold failed (exit %d)", code)
	}

	// finalizeInit likewise writes to io.Writer and returns 0/1.
	// Discard its narration; the HTTP response conveys structured
	// errors via the sentinel types.
	if code := finalizeInit(dir, io.Discard, io.Discard, initFinalizeOptions{
		skipProviderReadiness: req.SkipProviderReadiness,
		showProgress:          false,
		commandName:           "gc init",
	}); code != 0 {
		// finalizeInit's current contract is "blocked, check
		// stderr". Without a structured return type we can't map
		// to specific sentinels; future work splits this out.
		return nil, fmt.Errorf("finalize failed (exit %d)", code)
	}

	cityName := resolveCityName(req.NameOverride, dir)
	return &cityinit.InitResult{
		CityName:     cityName,
		CityPath:     dir,
		ProviderUsed: req.Provider,
	}, nil
}

// validateInitRequest performs the membership / mutual-exclusion
// checks that the HTTP layer previously did inline. Keeping them in
// the bridge means the CLI also benefits from the same validation
// when its call site moves (follow-up).
func validateInitRequest(req *cityinit.InitRequest) error {
	if req.Dir == "" {
		return fmt.Errorf("%w: dir is required", cityinit.ErrInvalidProvider)
	}
	if !filepath.IsAbs(req.Dir) {
		return fmt.Errorf("dir must be absolute: %q", req.Dir)
	}
	if req.Provider == "" && req.StartCommand == "" {
		return fmt.Errorf("%w: provider or start_command required", cityinit.ErrInvalidProvider)
	}
	if req.Provider != "" {
		if _, ok := config.BuiltinProviders()[req.Provider]; !ok {
			return fmt.Errorf("%w: unknown provider %q", cityinit.ErrInvalidProvider, req.Provider)
		}
	}
	if req.BootstrapProfile != "" {
		// normalizeBootstrapProfile accepts every spelling the CLI
		// and HTTP API currently support; reuse it here so the two
		// projections can't disagree.
		if _, err := normalizeBootstrapProfile(req.BootstrapProfile); err != nil {
			return fmt.Errorf("%w: %w", cityinit.ErrInvalidBootstrapProfile, err)
		}
	}
	return nil
}
