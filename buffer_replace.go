// Bulk prepare-then-commit pipeline used by /warm and /rebuild.
//
// The shape is: build a complete replacement state OFF the buffer
// (validate every item, assign seqs, compute size), then commit it
// atomically under b.mu in a single state-swap. This separation is
// what lets multi-scope /warm be all-or-nothing — every scope is
// validated and built before any is committed; if any scope fails
// validation the existing state is untouched.
//
// Two commit variants exist:
//
//   - commitReplacement: stand-alone commit; computes its byte delta
//     against the buffer's current b.bytes under the lock. Used by
//     replaceAll (single-scope test path).
//
//   - commitReplacementPreReserved: batch-aware commit used by
//     Store.replaceScopes. The batch has already CAS-reserved its net
//     delta against totalBytes; this variant reconciles drift caused
//     by concurrent writes between snapshot and commit, but does NOT
//     re-add the delta itself.
//
// The drift-handling math in commitReplacementPreReserved is
// correctness-sensitive — concurrent writes between snapshot and
// commit must reconcile against the batch's pre-reserved delta.
// Re-run TestStore_ReplaceScopes_RaceVsWipe and
// TestStore_ReplaceScopes_RaceVsRebuild under stress after changes.

package scopecache

import (
	"errors"
	"time"
)

// scopeReplacement holds a fully built scope state ready to be atomically
// swapped into a scopeBuffer. Separating "prepare" from "commit" lets callers
// like /warm and /rebuild validate every scope up-front and only mutate state
// once they know all scopes will succeed.
//
// idKeyBytes is computed during buildReplacementState (which already
// walks every item) and assigned wholesale at commit time, so the
// O(1) approxSizeBytesLocked path stays correct after a /warm or
// /rebuild without forcing the commit to re-walk byID.
//
// byUUID, firstUUID and lastUUID are built the same way: a /warm or
// /rebuild fully replaces the scope, so it also replaces the uuid
// index and span (the seq cursor resets too — see the comment in
// buildReplacementState).
type scopeReplacement struct {
	items      []*Item
	byID       map[string]*Item
	bySeq      map[uint64]*Item
	byUUID     map[string]*Item
	lastSeq    uint64
	firstUUID  string
	lastUUID   string
	idKeyBytes int64
}

// buildReplacementState converts a caller-supplied item list into the
// internal state a scope buffer can adopt atomically. Callers are expected
// to have already enforced the per-scope capacity; this function does not
// trim — if len(items) exceeds the cap it would simply build an over-full
// state. The capacity check lives in the Store layer so one place owns it.
//
// mintUUID controls the adopt-or-mint contract (see RFC): when true,
// an item arriving without a uuid has one minted. Store-attached
// callers pass true; an orphan test buffer passes false so its items
// stay uuid-less, consistent with insertNewItemLocked's b.store guard.
func buildReplacementState(items []Item, mintUUID bool) (scopeReplacement, error) {
	if len(items) == 0 {
		return scopeReplacement{
			items:  []*Item{},
			byID:   make(map[string]*Item),
			bySeq:  make(map[uint64]*Item),
			byUUID: make(map[string]*Item),
		}, nil
	}

	seen := make(map[string]struct{}, len(items))
	nonEmptyIDs := 0
	built := make([]*Item, 0, len(items))
	bySeq := make(map[uint64]*Item, len(items))

	// seq is a cache-local cursor that is NOT stable across /warm or /rebuild.
	// We regenerate it from 1 for every call so scope buffers have monotonic,
	// dense seq values even when the input items came from elsewhere.
	//
	// ts is cache-owned: every item in a /warm or /rebuild batch is stamped
	// with the same now() value. The cache cannot honestly recover "when did
	// this item originally arrive in the universe" from a rebuild input —
	// that's source-of-truth metadata. Stamping now() captures the only
	// time the cache itself can attest to: when it received this batch.
	nowUs := time.Now().UnixMicro()
	var lastSeq uint64
	for _, src := range items {
		if src.ID != "" {
			if _, ok := seen[src.ID]; ok {
				return scopeReplacement{}, errors.New("duplicate 'id' value within scope: '" + src.ID + "'")
			}
			seen[src.ID] = struct{}{}
			nonEmptyIDs++
		}

		lastSeq++
		item := src
		// Defensive clear of counter — public API boundaries strip
		// it; clear here for internal callers / tests that built an
		// Item directly. A smuggled non-nil cell would make
		// approxItemSize charge counterCellOverhead instead of
		// len(Payload), and post-warm reads would materialise from
		// the orphaned cell.
		item.counter = nil
		item.Seq = lastSeq
		item.Ts = nowUs
		// uuid: adopt the client-supplied value (validateWriteItem
		// already verified v7 shape on the /warm//rebuild path) or
		// mint a fresh one. !mintUUID (orphan test buffer) leaves it
		// empty, consistent with insertNewItemLocked.
		if item.UUID == "" && mintUUID {
			item.UUID = newUUIDv7()
		}
		// /warm and /rebuild's per-item validateWriteItem already filled
		// renderBytes for string payloads; recompute defensively for
		// internal callers / tests that bypass the validator.
		if item.renderBytes == nil {
			item.renderBytes = precomputeRenderBytes(item.Payload)
		}

		// One heap *Item shared by built and bySeq (and byID below).
		stored := &item
		built = append(built, stored)
		bySeq[item.Seq] = stored
	}

	byID := make(map[string]*Item, nonEmptyIDs)
	var idKeyBytes int64
	for _, item := range built {
		if item.ID != "" {
			byID[item.ID] = item
			idKeyBytes += int64(len(item.ID))
		}
	}

	// byUUID + the per-scope uuid span. A duplicate uuid within the
	// scope is a data error (the source supplied two rows with the
	// same identity) — reject the whole batch, mirroring the byID
	// duplicate check above.
	byUUID := make(map[string]*Item, len(built))
	for _, item := range built {
		if item.UUID == "" {
			continue
		}
		if _, dup := byUUID[item.UUID]; dup {
			return scopeReplacement{}, errors.New("duplicate 'uuid' value within scope: '" + item.UUID + "'")
		}
		byUUID[item.UUID] = item
	}

	return scopeReplacement{
		items:      built,
		byID:       byID,
		bySeq:      bySeq,
		byUUID:     byUUID,
		lastSeq:    lastSeq,
		firstUUID:  built[0].UUID,
		lastUUID:   built[len(built)-1].UUID,
		idKeyBytes: idKeyBytes,
	}, nil
}

// sumItemBytes returns the total approxItemSize across a flat item slice.
// Used by batch operations to compute per-plan newBytes before commit.
func sumItemBytes(items []*Item) int64 {
	var n int64
	for i := range items {
		n += approxItemSize(*items[i])
	}
	return n
}

// commitReplacement atomically swaps the scope's state and adjusts the store
// byte counter by the *actual* delta (newBytes - b.bytes at commit time).
// Reading b.bytes under b.mu here makes the commit robust against a
// concurrent /append that completed between the caller's pre-check and this
// commit: any bytes it added to the store counter are cancelled out by the
// fresh delta, because its item is being replaced anyway.
//
// The caller must have already validated and built the replacement
// via buildReplacementState. Both commit variants are infallible
// after that point — that's what lets the broader prepare-then-
// commit pipeline (see file header) give /warm and /rebuild their
// all-or-nothing semantics.
func (b *scopeBuffer) commitReplacement(r scopeReplacement, newBytes int64) {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := time.Now().UnixMicro()
	if b.store != nil {
		b.store.totalBytes.Add(newBytes - b.bytes)
		// itemDelta uses the CURRENT len(b.items) under b.mu, not a
		// pre-snapshot, so a stale-pointer concurrent /append that
		// landed between the caller's snapshot and this commit is
		// folded into the delta naturally — its +1 to totalItems is
		// undone here because its item is being discarded by the
		// swap. No drift parameter needed (unlike newBytes - oldSnapshot
		// for bytes, which is pre-reserved in the PreReserved variant).
		b.store.totalItems.Add(int64(len(r.items)) - int64(len(b.items)))
		b.store.bumpLastWriteTS(now)
	}
	b.bytes = newBytes
	b.idKeyBytes = r.idKeyBytes
	b.items = r.items
	b.byID = r.byID
	b.bySeq = r.bySeq
	b.byUUID = r.byUUID
	b.lastSeq = r.lastSeq
	b.firstUUID = r.firstUUID
	b.lastUUID = r.lastUUID
	b.lastWriteTS = now
}

// commitReplacementPreReserved is the batch-aware commit used by
// Store.replaceScopes. The caller has already atomically reserved
// (newBytes - oldSnapshot) bytes against the store counter via reserveBytes,
// so this commit must NOT re-add that delta; it only releases drift caused
// by concurrent writes to this scope between the snapshot and the commit,
// which keeps the store-wide byte cap strict across batch replacements.
//
// Drift handling, using oldSnapshot (b.bytes as read under RLock during
// the batch's cap check):
//
//   - Concurrent /append on this scope in the window: b.bytes grew by +X
//     and the appender did totalBytes.Add(+X). Drift = b.bytes - oldSnapshot
//     = X; we Add(-X), releasing that reservation (the appended item gets
//     discarded by the replacement anyway).
//   - Concurrent /delete on this scope in the window: b.bytes shrank by Y
//     and the deleter did totalBytes.Add(-Y). Drift is negative; Add(-drift)
//     is positive, compensating for the extra release so the scope's net
//     contribution to totalBytes is exactly (newBytes - oldSnapshot).
//   - No concurrent activity: drift = 0, no counter adjustment.
func (b *scopeBuffer) commitReplacementPreReserved(r scopeReplacement, newBytes int64, oldSnapshot int64) {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := time.Now().UnixMicro()
	if b.store != nil {
		drift := b.bytes - oldSnapshot
		if drift != 0 {
			b.store.totalBytes.Add(-drift)
		}
		// totalItems has no pre-reservation: len(b.items) under the
		// lock captures any concurrent /append's contribution
		// naturally — its item is being discarded by the swap.
		b.store.totalItems.Add(int64(len(r.items)) - int64(len(b.items)))
		b.store.bumpLastWriteTS(now)
	}
	b.bytes = newBytes
	b.idKeyBytes = r.idKeyBytes
	b.items = r.items
	b.byID = r.byID
	b.bySeq = r.bySeq
	b.byUUID = r.byUUID
	b.lastSeq = r.lastSeq
	b.firstUUID = r.firstUUID
	b.lastUUID = r.lastUUID
	b.lastWriteTS = now
}

func (b *scopeBuffer) replaceAll(items []Item) ([]Item, error) {
	if b.itemCapExceeded(len(items)) {
		return nil, &ScopeFullError{Count: len(items), Cap: b.maxItems}
	}
	r, err := buildReplacementState(items, b.store != nil)
	if err != nil {
		return nil, err
	}
	newBytes := sumItemBytes(r.items)
	b.commitReplacement(r, newBytes)

	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]Item, len(b.items))
	for i, p := range b.items {
		out[i] = *p
	}
	return out, nil
}
