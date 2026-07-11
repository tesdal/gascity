package beads

import (
	"errors"
	"fmt"
	"time"

	"github.com/gastownhall/gascity/internal/rollout/gate"
)

// This file holds CachingStore's ConditionalWriter forwarding. The cache rule
// for fenced writes is: forward, and evict — never patch. The unconditional
// write paths optimistically patch the cached clone when the post-write
// refresh fails; a conditional-write port of that fallback is poison, because
// the patch cannot synthesize the new revision, so every consumer's
// precondition recovery would re-read the stale revision through the cache
// and re-fail — a livelock indistinguishable from real contention. Eviction
// instead routes the next Get to the backing store (dirty-set + entry
// removal; NEVER a deletedSeq stamp, which would short-circuit Get to
// ErrNotFound without consulting the backing).
//
// On success with a working refresh, the cache adopts the fresh read and
// writes through exactly what the fenced verb proved committed — the caller's
// opts, the closed status, or the swapped metadata key — because backings
// with read visibility lag can serve the pre-write row on the refresh. The
// revision always comes from the fresh read, never synthesized: a lagged
// revision self-heals (a fenced write against it precondition-fails and
// evicts), whereas a lagged field value would leave this process blind to its
// own committed write.
//
// The write-through has a known advance hazard, accepted as the price of the
// lag defense: when the refresh observes a LATER state (another writer landed
// between our commit and our refresh), the write-through stomps that writer's
// value onto a current revision, producing a clean cache entry the fence
// cannot self-heal (fenced writes succeed against it). Lag and advance are
// indistinguishable here without revision arithmetic, which the
// ConditionalWriter granularity contract forbids. The fabrication heals via
// the reconciler's content diff, bounded by the recent-local-mutation
// conflict window.
var (
	_ ConditionalWriter                = (*CachingStore)(nil)
	_ conditionalWritesModeCarrier     = (*CachingStore)(nil)
	_ conditionalWriteCapabilityProber = (*CachingStore)(nil)
)

// The cache is a wrapper, not a second store, so it carries no
// conditional-writes stamp of its own (§6.3): the stamp, its read, and the
// degrade latch all delegate to the backing store. A backing that cannot
// carry a stamp (a wrapped or cross-package store) leaves the pair at
// ModeUnset, so the seam takes the legacy path — enforcement is never raised
// through a cache whose backing cannot express the mode.

// stampConditionalWritesMode forwards the factory stamp to the backing store
// and reports whether it landed there; false (carrier-less backing) tells the
// factory the mode was dropped so the miss is logged, never silently believed.
func (c *CachingStore) stampConditionalWritesMode(mode gate.Mode, defaulted bool) bool {
	if carrier, ok := c.backing.(conditionalWritesModeCarrier); ok {
		return carrier.stampConditionalWritesMode(mode, defaulted)
	}
	return false
}

// conditionalWritesMode reads the backing store's stamp.
func (c *CachingStore) conditionalWritesMode() (gate.Mode, bool) {
	if carrier, ok := c.backing.(conditionalWritesModeCarrier); ok {
		return carrier.conditionalWritesMode()
	}
	return gate.ModeUnset, false
}

// noteConditionalDegradeOnce shares the backing store's degrade latch: cache
// and backing are one store instance for emission purposes.
func (c *CachingStore) noteConditionalDegradeOnce() bool {
	if carrier, ok := c.backing.(conditionalWritesModeCarrier); ok {
		return carrier.noteConditionalDegradeOnce()
	}
	return false
}

// probeConditionalWriteCapability answers with the backing store's capability:
// the cache's own ConditionalWriter verbs forward to the backing, so its
// capability IS the backing's. A backing with CAS verbs but no prober is
// vacuously capable, mirroring the seam's default.
func (c *CachingStore) probeConditionalWriteCapability() (bool, string) {
	if prober, ok := c.backing.(conditionalWriteCapabilityProber); ok {
		return prober.probeConditionalWriteCapability()
	}
	if _, ok := ConditionalWriterFor(c.backing); ok {
		return true, ""
	}
	return false, "backing store does not implement conditional writes"
}

// UpdateIfMatch forwards the fenced update to the backing store's conditional
// writer and maintains the cache: refresh on success, evict when the refresh
// fails or the precondition does. A backing without the capability yields
// ErrConditionalWriteUnsupported — never an unconditional write.
func (c *CachingStore) UpdateIfMatch(id string, expectedRevision int64, opts UpdateOpts) error {
	writer, ok := ConditionalWriterFor(c.backing)
	if !ok {
		return ErrConditionalWriteUnsupported
	}
	if err := writer.UpdateIfMatch(id, expectedRevision, opts); err != nil {
		c.applyConditionalWriteFailure(id, err)
		return err
	}
	fresh, refreshed := c.refreshBeadAfterWrite(id, "refresh bead after conditional update")
	if !refreshed {
		c.evictForConditionalWrite(id)
		return nil
	}
	fresh = applyUpdateOptsToBead(fresh, opts)
	c.adoptConditionalRefresh(id, fresh, opts.Status != nil)
	c.notifyChange("bead.updated", fresh)
	return nil
}

// CloseIfMatch forwards the fenced close and maintains the cache. A post-close
// refresh that reports ErrNotFound is tolerated silently — backings that hide
// closed beads from Get do this on every successful close — and resolves to an
// evict, so the next read reports exactly what the backing itself would.
// Unlike the unconditional Close, a fenced re-close of an already-closed bead
// is not suppressed and re-fires bead.closed: fenced paths carry no
// idempotence short-circuits, and only the backing evaluates the fence.
func (c *CachingStore) CloseIfMatch(id string, expectedRevision int64) error {
	writer, ok := ConditionalWriterFor(c.backing)
	if !ok {
		return ErrConditionalWriteUnsupported
	}
	if err := writer.CloseIfMatch(id, expectedRevision); err != nil {
		c.applyConditionalWriteFailure(id, err)
		return err
	}
	fresh, err := c.backing.Get(id)
	if err != nil {
		if !errors.Is(err, ErrNotFound) {
			c.recordProblem("refresh bead after conditional close", fmt.Errorf("%s: %w", id, err))
		}
		c.evictForConditionalWrite(id)
		return nil
	}
	fresh.Status = "closed"
	c.adoptConditionalRefresh(id, fresh, true)
	c.notifyChange("bead.closed", fresh)
	return nil
}

// DeleteIfMatch forwards the fenced delete and, on success, mirrors the
// unconditional Delete's full scrub — the one place the deletedSeq stamp is
// correct, because the bead is actually gone.
func (c *CachingStore) DeleteIfMatch(id string, expectedRevision int64) error {
	writer, ok := ConditionalWriterFor(c.backing)
	if !ok {
		return ErrConditionalWriteUnsupported
	}
	deleted, haveDeleted := c.snapshotBeadBeforeDelete(id)
	if err := writer.DeleteIfMatch(id, expectedRevision); err != nil {
		c.applyConditionalWriteFailure(id, err)
		return err
	}

	c.mu.Lock()
	seq := c.noteLocalMutationLocked(id)
	delete(c.beads, id)
	delete(c.deps, id)
	delete(c.dirty, id)
	delete(c.beadSeq, id)
	delete(c.localBeadAt, id)
	c.deletedSeq[id] = seq
	c.clearDependentReadyProjectionsLocked(id)
	c.markFreshLocked(time.Now())
	c.updateStatsLocked()
	c.mu.Unlock()
	if haveDeleted {
		c.notifyChange("bead.deleted", deleted)
	}
	return nil
}

// CompareAndSetMetadataKey forwards the metadata CAS. There is deliberately no
// cached-value pre-check: only the backing evaluates the fence, and a cached
// value-match proves nothing about the revision. A clean value-loss
// (false, nil) evicts too — the cached value fed this process its losing
// `expected`, and without the evict a cross-process loser re-reads the same
// stale value through the cache and re-loses until an unrelated reconcile.
func (c *CachingStore) CompareAndSetMetadataKey(id, key, expected, next string) (bool, error) {
	writer, ok := ConditionalWriterFor(c.backing)
	if !ok {
		return false, ErrConditionalWriteUnsupported
	}
	swapped, err := writer.CompareAndSetMetadataKey(id, key, expected, next)
	if err != nil {
		c.applyConditionalWriteFailure(id, err)
		return swapped, err
	}
	if !swapped {
		c.evictForConditionalWrite(id)
		return false, nil
	}
	fresh, refreshed := c.refreshBeadAfterWrite(id, "refresh bead after conditional metadata swap")
	if !refreshed {
		c.evictForConditionalWrite(id)
		return true, nil
	}
	if fresh.Metadata == nil {
		fresh.Metadata = make(map[string]string, 1)
	}
	fresh.Metadata[key] = next
	c.adoptConditionalRefresh(id, fresh, false)
	c.notifyChange("bead.updated", fresh)
	return true, nil
}

// applyConditionalWriteFailure maps the backing writer's error class onto the
// cache action it dictates. A precondition failure proves the cached revision
// stale → evict. Gate refusal, CAS exhaustion, and unsupported prove the write
// did not commit and nothing about this entry's freshness → no action.
// Anything else (transport failures, not-found, ambiguous may-have-committed
// errors) marks the entry dirty: the next Get re-reads the backing and
// re-primes, without dropping the entry from cached listings. The error itself
// is always returned to the caller untouched — the backing stores stamp
// ID/Expected/Current; this layer adds cache maintenance, not decoration.
func (c *CachingStore) applyConditionalWriteFailure(id string, err error) {
	switch {
	case IsPreconditionFailed(err):
		c.evictForConditionalWrite(id)
	case IsGateRefusal(err), IsCASRetriesExhausted(err), IsConditionalWriteUnsupported(err):
	default:
		// noteLocalMutationLocked bumps the mutation seq so a scan that
		// started before this failure cannot merge its pre-write row back
		// over the mark and delete it.
		c.mu.Lock()
		c.noteLocalMutationLocked(id)
		c.dirty[id] = struct{}{}
		c.mu.Unlock()
	}
}

// evictForConditionalWrite removes the cached entry so the next Get re-reads
// the backing store and re-primes (the dirty flag routes it there).
// noteLocalMutationLocked keeps a concurrent scan's merge-back from
// re-installing its stale row as CLEAN; prime's concurrent-mutation branch
// can still re-add a stale row for the missing id, but it leaves the dirty
// flag intact — the flag, not the entry's absence, is what keeps readers off
// stale state, so do not "simplify" the dirty-set away. deletedSeq is never
// stamped here: the bead still exists, and deletedSeq short-circuits Get to
// ErrNotFound without ever consulting the backing.
func (c *CachingStore) evictForConditionalWrite(id string) {
	c.mu.Lock()
	c.noteLocalMutationLocked(id)
	delete(c.beads, id)
	delete(c.deps, id)
	c.dirty[id] = struct{}{}
	c.clearDependentReadyProjectionsLocked(id)
	c.markFreshLocked(time.Now())
	c.updateStatsLocked()
	c.mu.Unlock()
}

// adoptConditionalRefresh installs the post-write backing state into the
// cache — the same bookkeeping the unconditional write paths perform after a
// successful refresh.
func (c *CachingStore) adoptConditionalRefresh(id string, fresh Bead, statusChanged bool) {
	c.mu.Lock()
	c.noteLocalMutationLocked(id)
	c.beads[id] = cloneBead(fresh)
	c.deps[id] = depsFromBeadFields(fresh)
	if statusChanged {
		c.clearDependentReadyProjectionsLocked(id)
	}
	delete(c.dirty, id)
	delete(c.deletedSeq, id)
	c.markFreshLocked(time.Now())
	c.updateStatsLocked()
	c.mu.Unlock()
}
