// The conditional-writes resolution seam: the single tested composition point
// of operator policy (the factory-stamped beads.conditional_writes mode) and
// runtime capability (per-store probes). Consumers call
// ResolveConditionalWriter(store) and never see a mode value — the mode is
// stamped onto the store by the beads factory (OpenStoreAtForCity), so a
// caller cannot contradict the store (DESIGN §6.3/§6.4).
//
// The mode type and the enable-AND-capable product live in
// internal/rollout/gate — the dependency-leaf half of internal/rollout —
// because this package cannot import internal/rollout itself
// (rollout → config → orders → beads would cycle).
//
// The seam's diagnostic return is a fresh BeadsDiagnostic value describing the
// degrade/refusal; it reuses the existing PreflightGate/PreflightReason fields
// and NEVER rides the status wire (StatusResponse's beads diagnostic comes from
// StoreOpenResult.Diagnostic exclusively). Keep it that way until the §12.5
// per-store status verdicts land.

package beads

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/gastownhall/gascity/internal/rollout/gate"
)

// conditionalWritesGate is the diagnostic gate label for every conditional-
// writes degrade and refusal surface (mirrors the flag key's last segment).
const conditionalWritesGate = "conditional_writes"

// condWritesStamp is the factory-stamped conditional-writes state carried by
// every package-beads store type as embedded instance state. The factory
// stamps it once at open; the seam reads it on every resolve; the degrade
// latch arms the (stage-3) once-per-store degraded event. All three fields
// share one mutex so the stamp is correct under races: not every store is
// stamped strictly before sharing (the t3bridge watcher path constructs and
// shares stores concurrently), and noteConditionalDegradeOnce mutates at
// resolve time, not construction time.
//
// The stamp is deliberately Mode-only: the value's Origin (builtin|config|env)
// is composition-root knowledge and travels with the stage-3 degraded-event
// emission, never with the store.
type condWritesStamp struct {
	condWritesMu sync.Mutex
	// condWritesModeVal is the resolved city-global mode, latched for the
	// store's lifetime. Zero (ModeUnset) means no factory threaded a mode:
	// the seam treats it exactly like Off, so an unwired open path can never
	// RAISE enforcement.
	condWritesModeVal gate.Mode
	// condWritesDefaulted records that the factory received ModeUnset and
	// mapped it to Off — an unthreaded open path, distinguishable from a
	// deliberate off for tests and future doctor surfaces.
	condWritesDefaulted bool
	// condWritesDegradeNoted arms at-most-once degraded-event emission per
	// store instance (stage 3 wires the emitter; S2b ships the latch unwired).
	condWritesDegradeNoted bool
}

// stampConditionalWritesMode records the resolved mode on this store.
// defaulted marks a ModeUnset→Off factory mapping (unthreaded open path). The
// return reports whether the stamp LANDED: a store that owns its stamp always
// lands it; a delegating wrapper (CachingStore) forwards into its backing and
// reports false when the backing cannot carry a mode — the factory logs that
// miss instead of believing the stamp took (red-team F2: a silently dropped
// require stamp is the silent-fallback shape §6.4 exists to kill).
func (s *condWritesStamp) stampConditionalWritesMode(mode gate.Mode, defaulted bool) bool {
	s.condWritesMu.Lock()
	defer s.condWritesMu.Unlock()
	s.condWritesModeVal = mode
	s.condWritesDefaulted = defaulted
	return true
}

// conditionalWritesMode returns the stamped mode and whether it was a
// factory default for an unthreaded open path.
func (s *condWritesStamp) conditionalWritesMode() (gate.Mode, bool) {
	s.condWritesMu.Lock()
	defer s.condWritesMu.Unlock()
	return s.condWritesModeVal, s.condWritesDefaulted
}

// noteConditionalDegradeOnce reports true exactly once per store instance —
// the first capability degrade — so the stage-3 emitter can fire the
// beads.conditional_writes.degraded event without log storms. The seam does
// not consult it in S2b (the diagnostic is returned on EVERY degrade call so
// resolution stays order-independent); it exists for the emission wiring.
func (s *condWritesStamp) noteConditionalDegradeOnce() bool {
	s.condWritesMu.Lock()
	defer s.condWritesMu.Unlock()
	if s.condWritesDegradeNoted {
		return false
	}
	s.condWritesDegradeNoted = true
	return true
}

// conditionalWritesModeCarrier is the unexported stamp surface: only
// internal/beads types can implement it, so no consumer can synthesize a
// differently-moded store (DESIGN §6.4). Store types embed condWritesStamp to
// satisfy it; CachingStore forwards to its backing store instead of carrying
// its own stamp. exec.Store (a separate package) deliberately does NOT carry
// a stamp: it implements no conditional writes, so an exec store resolves as
// ModeUnset→legacy — enforcement can never be raised on an unstamped path.
type conditionalWritesModeCarrier interface {
	// stampConditionalWritesMode records the mode, reporting whether the
	// stamp landed on a store that can carry it (false = forwarded into a
	// carrier-less backing; the caller must surface the miss).
	stampConditionalWritesMode(mode gate.Mode, defaulted bool) bool
	conditionalWritesMode() (gate.Mode, bool)
	noteConditionalDegradeOnce() bool
}

// conditionalWriteCapabilityProber is the per-store capability answer for the
// seam. Implementations must be cheap OR lazily memoized: the seam consults
// the prober only under Auto/Require (Off is zero-cost by contract), but a
// resolve can happen on hot paths. A store that implements ConditionalWriter
// without a prober is vacuously capable (mirroring gate.ResolveCapability's
// nil-predicate rule); a store that lacks ConditionalWriter entirely is
// incapable regardless of any prober.
type conditionalWriteCapabilityProber interface {
	// probeConditionalWriteCapability reports whether conditional writes can
	// succeed on this store instance, with a human-readable reason when not.
	probeConditionalWriteCapability() (capable bool, reason string)
}

// ConditionalWritesRequiredError reports that the resolved store cannot
// perform conditional writes while the factory-stamped mode is require: the
// caller must fail closed — retrying, surfacing, or stalling — and MUST NOT
// fall back to an unconditional write. It is resolve-time and store-scoped,
// never per-bead. Origin is deliberately absent: the stamp carries Mode only;
// origin is attached where the composition root holds the resolved Flags.
type ConditionalWritesRequiredError struct {
	// StoreKind names the refusing store type (BdStore, MemStore, ...).
	StoreKind string
	// Reason carries the capability probe's explanation verbatim.
	Reason string
}

// Error reports the refusal in the §12.3 diagnostic grammar.
func (e *ConditionalWritesRequiredError) Error() string {
	if e == nil {
		return "<nil>"
	}
	return fmt.Sprintf("conditional_writes refused: store=%s mode=require reason=%q", e.StoreKind, e.Reason)
}

// IsConditionalWritesRequired reports whether err is or wraps a
// *ConditionalWritesRequiredError.
func IsConditionalWritesRequired(err error) bool {
	var cre *ConditionalWritesRequiredError
	return errors.As(err, &cre)
}

// ResolveConditionalWriter is the single composition point of the
// factory-stamped beads.conditional_writes mode and per-store runtime
// capability. There is no mode parameter: the mode is read from the store's
// stamp, so callers cannot contradict the store. The return contract:
//
//	off / unset (or unstamped store)  -> (nil, nil, nil): take the
//	    byte-identical legacy write path. No capability probe runs.
//	auto ∧ capable / require ∧ capable -> (writer, nil, nil): the writer is
//	    the RESOLVED store itself (a CachingStore resolves to the CachingStore,
//	    preserving its forward-and-evict cache rules — never its backing).
//	auto ∧ incapable                  -> (nil, diagnostic, nil): take the
//	    legacy path AND surface the diagnostic (loud degrade). The diagnostic
//	    is returned on every call, deterministically.
//	require ∧ incapable               -> (nil, diagnostic, typed refusal):
//	    fail closed; never fall back to an unconditional write.
//
// Like ConditionalWriterFor, the seam does not unwrap store wrappers: a
// caller holding a typed class wrapper or a cmd/gc policy wrapper must
// resolve the unwrapped store (interface embedding does not promote the
// stamp, so a wrapped resolve degrades to unset→legacy).
func ResolveConditionalWriter(store Store) (ConditionalWriter, *BeadsDiagnostic, error) {
	mode := gate.ModeUnset
	if carrier, ok := store.(conditionalWritesModeCarrier); ok {
		mode, _ = carrier.conditionalWritesMode()
	}
	if mode == gate.ModeUnset || mode == gate.Off {
		return nil, nil, nil
	}

	writer, hasWriter := ConditionalWriterFor(store)
	pred := func(context.Context) (bool, string) {
		if !hasWriter {
			return false, "store does not implement conditional writes"
		}
		if prober, ok := store.(conditionalWriteCapabilityProber); ok {
			return prober.probeConditionalWriteCapability()
		}
		return true, "store implements conditional writes"
	}

	decision, reason := gate.ResolveCapability(context.Background(), mode, pred)
	switch decision {
	case gate.UseNew:
		if writer == nil {
			// Unreachable by construction (the predicate reports incapable
			// when hasWriter is false), but stay mode-correct if it ever
			// happens: require still fails closed, auto degrades loudly.
			return refuseOrDegrade(store, mode, "store did not yield a conditional writer")
		}
		return writer, nil, nil
	case gate.DegradeLoud, gate.RefuseClosed:
		return refuseOrDegrade(store, mode, reason)
	default: // gate.UseLegacy — unreachable: off/unset short-circuit above.
		return nil, nil, nil
	}
}

// refuseOrDegrade builds the incapable-store outcome for the resolved mode:
// a loud-degrade diagnostic under auto, the same diagnostic plus the typed
// fail-closed refusal under require.
func refuseOrDegrade(store Store, mode gate.Mode, reason string) (ConditionalWriter, *BeadsDiagnostic, error) {
	kind := conditionalStoreKind(store)
	diag := &BeadsDiagnostic{
		Store:           kind,
		PreflightGate:   conditionalWritesGate,
		PreflightReason: fmt.Sprintf("mode=%s: %s", mode, reason),
	}
	if mode == gate.Require {
		return nil, diag, &ConditionalWritesRequiredError{StoreKind: kind, Reason: reason}
	}
	return nil, diag, nil
}

// conditionalStoreKind names the store type for diagnostics. Types that only
// exist under build tags (DoltliteReadStore) and test doubles fall through to
// the %T spelling, which is descriptive enough for a diagnostic surface.
func conditionalStoreKind(store Store) string {
	switch store.(type) {
	case *BdStore:
		return storeNameBdStore
	case *FileStore:
		return storeNameFileStore
	case *MemStore:
		return "MemStore"
	case *CachingStore:
		return "CachingStore"
	case *NativeDoltStore:
		return storeNameNativeDoltStore
	case nil:
		return "<nil>"
	default:
		return fmt.Sprintf("%T", store)
	}
}
