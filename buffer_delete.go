// Delete paths on *scopeBuffer:
//
//   - deleteByID     — single-item delete by id
//   - deleteBySeq    — single-item delete by seq
//   - deleteUpToSeq  — drain the prefix [seq=1 .. seq=maxSeq] in one shot
//
// All three take b.mu exclusively, check b.detached first, and route
// byte releases through the store-wide totalBytes counter. The
// low-level helper deleteIndexLocked centralises the GC-zeroing,
// secondary-index sync, and counter update so the three callers cannot
// drift.

package scopecache

import "sort"

// deleteIndexLocked removes items[i] in O(n) tail-shift, GC-zeroes
// the now-duplicate last slot, syncs bySeq + byID, and releases the
// item's bytes from b.bytes and (when store-attached) s.totalBytes.
//
// PRECONDITION: caller holds b.mu and i is valid.
//
// Three invariants the body upholds — drift here leaks state silently:
//
//  1. Zero the duplicate last slot before reslicing, otherwise the
//     backing array keeps a reference to the removed payload bytes.
//  2. b.bytes and s.totalBytes update lockstep.
//  3. byID delete is conditional on removed.ID != "" — empty-id
//     items have no byID entry to remove.
func (b *scopeBuffer) deleteIndexLocked(i int) {
	removed := b.items[i]
	removedSize := approxItemSize(removed)

	// Tail-shift then zero the now-duplicate last slot before
	// shrinking. Without the zero the backing array keeps a
	// reference to the removed Item (and its payload bytes) and
	// prevents GC.
	copy(b.items[i:], b.items[i+1:])
	b.items[len(b.items)-1] = Item{}
	b.items = b.items[:len(b.items)-1]

	delete(b.bySeq, removed.Seq)
	if removed.ID != "" {
		delete(b.byID, removed.ID)
		b.idKeyBytes -= int64(len(removed.ID))
	}

	b.bytes -= removedSize
	now := nowUnixMicro()
	if b.store != nil {
		b.store.totalBytes.Add(-removedSize)
		b.store.totalItems.Add(-1)
		b.store.bumpLastWriteTS(now)
	}
	b.lastWriteTS = now
	b.resetIfEmptyLocked()
	b.shrinkIfSparseLocked()
}

// shrinkIfSparseLocked rebuilds the items slice and the bySeq/byID
// maps when the slice has gone substantially sparse — defined as
// cap > shrinkMinCap AND len < cap/shrinkSparseRatio. Without this,
// a drain that reduces a 100k-item scope to a handful keeps the
// 100k-element slice backing array and the corresponding map
// bucket arrays alive (Go maps don't shrink on delete).
//
// Conservative thresholds — only fires on visibly-sparse buffers,
// not on minor churn. After a rebuild the new slice cap equals len,
// so a follow-up delete must again accumulate enough sparsity
// before another rebuild fires; amortised cost is O(1) per delete.
//
// PRECONDITION: caller holds b.mu and the delete logic has already
// updated b.bytes / counters.
const (
	shrinkMinCap      = 1024
	shrinkSparseRatio = 4 // len < cap/4 → rebuild
)

func (b *scopeBuffer) shrinkIfSparseLocked() {
	if cap(b.items) <= shrinkMinCap {
		return
	}
	if len(b.items)*shrinkSparseRatio >= cap(b.items) {
		return
	}
	// Rebuild items slice with cap = len so the high-water backing
	// array becomes GC-eligible.
	newItems := make([]Item, len(b.items))
	copy(newItems, b.items)
	// Rebuild maps from the surviving items so map bucket arrays
	// also shrink. Pre-size by len(newItems) — a slight over-
	// allocation when many items have empty IDs is cheaper than
	// growing the map back up via subsequent appends.
	newBySeq := make(map[uint64]Item, len(newItems))
	var newByID map[string]Item
	for i := range newItems {
		newBySeq[newItems[i].Seq] = newItems[i]
		if newItems[i].ID != "" {
			if newByID == nil {
				newByID = make(map[string]Item, len(newItems))
			}
			newByID[newItems[i].ID] = newItems[i]
		}
	}
	b.items = newItems
	b.bySeq = newBySeq
	b.byID = newByID
}

// resetIfEmptyLocked drops the high-watermark backing storage when a
// scope has just been drained to zero items. Without it, drained
// scopes retain the slice's full backing array and the maps' bucket
// arrays (Go maps don't shrink on delete) until the next write
// grows them.
//
// nil-ing is safe: write paths lazy-init the maps on first write
// after a reset, and append() on a nil slice grows naturally.
// b.lastSeq is intentionally not reset — the seq cursor must stay
// monotonic across drain/refill cycles so downstream consumers
// tracking it cannot observe a regression.
//
// PRECONDITION: caller holds b.mu and the delete that produced the
// empty state has already updated b.bytes / counters.
func (b *scopeBuffer) resetIfEmptyLocked() {
	if len(b.items) != 0 {
		return
	}
	b.items = nil
	b.bySeq = nil
	b.byID = nil
	// b.idKeyBytes is already zero — every removed item subtracted its
	// id length on delete; the explicit assignment is a defensive
	// guard against future delete-paths that forget the subtract.
	b.idKeyBytes = 0
}

func (b *scopeBuffer) deleteByID(id string) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.detached {
		return 0, &ScopeDetachedError{}
	}

	existing, ok := b.byID[id]
	if !ok {
		return 0, nil
	}

	i, ok := b.indexBySeqLocked(existing.Seq)
	if !ok {
		// Defensive: byID and items stay in sync under b.mu.
		return 0, nil
	}
	b.deleteIndexLocked(i)
	return 1, nil
}

func (b *scopeBuffer) deleteBySeq(seq uint64) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.detached {
		return 0, &ScopeDetachedError{}
	}

	if _, ok := b.bySeq[seq]; !ok {
		return 0, nil
	}

	i, ok := b.indexBySeqLocked(seq)
	if !ok {
		// Defensive: bySeq and items stay in sync under b.mu.
		return 0, nil
	}
	b.deleteIndexLocked(i)
	return 1, nil
}

// deleteUpToSeq removes every item with Seq <= maxSeq. b.items is
// always ordered ascending by Seq — appendItem assigns monotonic
// seqs, and the delete paths preserve relative order of the
// remaining items — so binary search finds the cut point in
// O(log n). Returns the number of items removed and any
// *ScopeDetachedError if the buffer was orphaned by /delete_scope,
// /wipe, or /rebuild before the caller's mutation could land.
func (b *scopeBuffer) deleteUpToSeq(maxSeq uint64) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.detached {
		return 0, &ScopeDetachedError{}
	}

	idx := sort.Search(len(b.items), func(i int) bool {
		return b.items[i].Seq > maxSeq
	})
	if idx == 0 {
		return 0, nil
	}

	var freedBytes int64
	var freedIDKeyBytes int64
	for i := 0; i < idx; i++ {
		removed := b.items[i]
		freedBytes += approxItemSize(removed)
		delete(b.bySeq, removed.Seq)
		if removed.ID != "" {
			delete(b.byID, removed.ID)
			freedIDKeyBytes += int64(len(removed.ID))
		}
	}
	// Copy the kept suffix into a fresh backing array so the old one —
	// which still holds the removed payloads in its prefix — becomes
	// GC-eligible. A bare reslice (b.items[idx:]) would pin the full
	// original array behind a small remainder; this matters for the
	// write-buffer pattern where repeated drain-from-front otherwise
	// retains memory proportional to the historical high-watermark.
	rest := make([]Item, len(b.items)-idx)
	copy(rest, b.items[idx:])
	b.items = rest

	b.bytes -= freedBytes
	b.idKeyBytes -= freedIDKeyBytes
	now := nowUnixMicro()
	if b.store != nil {
		b.store.totalBytes.Add(-freedBytes)
		b.store.totalItems.Add(-int64(idx))
		b.store.bumpLastWriteTS(now)
	}
	b.lastWriteTS = now
	// `rest` already freed the items-slice backing array; the reset
	// still matters for the maps (their buckets don't shrink on
	// delete) when this drain emptied the scope.
	b.resetIfEmptyLocked()
	return idx, nil
}
