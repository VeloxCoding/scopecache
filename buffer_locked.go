// Cross-cutting helpers shared by scopeBuffer's mutation paths. The
// `*Locked` suffix signals: caller MUST hold b.mu. These helpers
// never lock internally. precomputeRenderBytes is pure and
// lock-agnostic.
//
// The helpers centralise bytes-accounting on payload-replace.
// Secondary-index "sync" is not one of them — items, byID and bySeq
// alias one *Item per entry, so an in-place field write is visible
// through every index — but a forgotten bytes-accounting update
// still leaks state silently.

package scopecache

import (
	"bytes"
	"encoding/json"
	"sort"
)

// precomputeRenderBytes returns the JSON-string-decoded form of payload
// when payload's first non-whitespace byte is `"`, or nil otherwise.
// Called at write-time so /render hits skip the per-call json.Unmarshal
// + []byte cast. Returns nil on a malformed JSON string (defensive — the
// validator already rejects malformed JSON on writes).
func precomputeRenderBytes(payload json.RawMessage) []byte {
	trimmed := bytes.TrimLeft(payload, " \t\r\n")
	if len(trimmed) == 0 || trimmed[0] != '"' {
		return nil
	}
	var s string
	if err := jsonUnmarshal(payload, &s); err != nil {
		return nil
	}
	return []byte(s)
}

// reservePayloadDeltaLocked reserves (newSize − oldSize) against the
// store-wide byte budget when delta != 0 and returns the delta so the
// caller can update b.bytes consistently after a successful mutation.
//
// PRECONDITION: caller holds b.mu.
//
// Returns *StoreFullError when the reservation fails — no store
// state mutated, caller returns the error without rollback.
func (b *scopeBuffer) reservePayloadDeltaLocked(oldSize, newSize int64) (int64, error) {
	delta := newSize - oldSize
	if delta != 0 {
		ok, current, max := b.store.reserveBytes(delta)
		if !ok {
			return 0, &StoreFullError{StoreBytes: current, AddedBytes: delta, Cap: max}
		}
	}
	return delta, nil
}

// itemCapExceeded reports whether `proposed` items would violate the
// per-scope item-count cap (b.store.defaultMaxItems).
//
// Single-item callers pass len(b.items) + 1; bulk callers pass the
// proposed batch size.
//
// Lock-agnostic: b.store.defaultMaxItems is set once at store
// construction and never reassigned. Callers that also read
// len(b.items) still hold b.mu so the (proposed, len) pair is
// consistent.
func (b *scopeBuffer) itemCapExceeded(proposed int) bool {
	return proposed > b.store.defaultMaxItems
}

// payloadAndRenderBytes returns the byte cost approxItemSize charges
// for an item's payload-related fields. Used by replace paths
// (upsert/update) to compute size deltas correctly.
func payloadAndRenderBytes(item *Item) int64 {
	return int64(len(item.Payload)) + int64(len(item.renderBytes))
}

// replaceItemAtIndexLocked overwrites payload + ts + renderBytes at
// items[i], applies delta to b.bytes, and stamps lastWriteTS.
//
// PRECONDITION: caller holds b.mu and i is valid — callers derive i
// from a checked lookup (indexBySeqLocked or a guaranteed-hit byID
// resolution) just before the call.
//
// No secondary-index re-sync: items[i], bySeq and byID all hold the
// same *Item, so the field writes below are visible through every
// index at once. Ts is always the caller-supplied value (every caller
// stamps fresh under b.mu); the "always refresh" rule lives at the
// call site, not here. renderBytes is caller-supplied because the
// size delta the caller passed in already accounts for it.
func (b *scopeBuffer) replaceItemAtIndexLocked(i int, payload json.RawMessage, ts int64, renderBytes []byte, delta int64) {
	b.items[i].Payload = payload
	b.items[i].Ts = ts
	b.items[i].renderBytes = renderBytes
	b.bytes += delta
	b.lastWriteTS = ts
	b.store.bumpLastWriteTS(ts)
}

// indexBySeqLocked returns the slice position of the item with seq in
// b.items via O(log n) binary search. Returns (0, false) on miss.
// items is always ordered ascending by seq (appendItem assigns
// monotonic seqs, nothing inserts in the middle), which is what makes
// the search valid.
//
// PRECONDITION: caller holds b.mu (read or write).
func (b *scopeBuffer) indexBySeqLocked(seq uint64) (int, bool) {
	i := sort.Search(len(b.items), func(i int) bool {
		return b.items[i].Seq >= seq
	})
	if i == len(b.items) || b.items[i].Seq != seq {
		return 0, false
	}
	return i, true
}
