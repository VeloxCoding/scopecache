// Read paths on *scopeBuffer:
//
//   - tailOffset  — window at the newest end, oldest-first within (drives /tail)
//   - sinceSeq    — window after a seq cursor, oldest-first (drives /head)
//   - getByID     — single-item lookup by id (drives /get?id=, /render)
//   - getBySeq    — single-item lookup by seq (drives /get?seq=)
//
// All four take b.mu.RLock so multiple readers run concurrently. None
// check b.detached: reading from a detached buffer returns the state
// the buffer had at detach time, which is fine for reads — no
// orphan-write hazard, only an eventually-stale snapshot. The
// read-bookkeeping (recordRead) runs separately and is lock-free.

package scopecache

import "sort"

// tailOffset returns the window of newest `limit` items after
// skipping `offset` from the newest end, appended into `out`. out
// MAY be nil or empty (a pool-borrowed scratch slice is fine); the
// method always returns out[:0] sliced to the actual result length.
// The window is preserved in its native seq-ascending (oldest-first)
// order; clients sort by seq if they want newest-first.
//
// hasMore is true when older items exist before the window (i.e.
// start > 0), signalling to the caller that the response is clipped
// at the oldest end. It does NOT signal truncation at the newest end
// (that is what offset already describes to the client).
//
// Scratch-slice contract: out may be a pool-borrowed slice with
// cap ≥ limit to avoid the per-call alloc that dominates GC pressure
// on the /head + /tail hot path (45% of bench-measured allocation
// space pre-pool). Caller is responsible for releasing out back to
// its pool after consuming the result.
func (b *scopeBuffer) tailOffset(out []Item, limit, offset int) ([]Item, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	if limit <= 0 || offset < 0 {
		return out[:0], false
	}
	if offset >= len(b.items) {
		return out[:0], false
	}

	end := len(b.items) - offset
	start := end - limit
	hasMore := start > 0
	if start < 0 {
		start = 0
	}
	if start >= end {
		return out[:0], false
	}

	// items is []*Item; deref-copy the window into out so the read path
	// never hands back the live pointers.
	window := b.items[start:end]
	out = out[:0]
	for _, p := range window {
		out = append(out, *p)
	}
	return out, hasMore
}

// sinceSeq returns items with seq > afterSeq, oldest-first, up to
// limit, appended into `out`. Same scratch-slice contract as
// tailOffset. limit ≤ 0 returns out[:0] — matches every other
// multi-item read on the public surface. HTTP rejects 0/negative
// with 400 via normalizeLimit; the guard here exists for direct
// Gateway callers.
//
// The bool is true when more matching items exist beyond the
// returned slice, so the handler surfaces truncated=true without
// the client guessing from count == limit.
func (b *scopeBuffer) sinceSeq(out []Item, afterSeq uint64, limit int) ([]Item, bool) {
	if limit <= 0 {
		return out[:0], false
	}

	b.mu.RLock()
	defer b.mu.RUnlock()

	if len(b.items) == 0 {
		return out[:0], false
	}

	idx := sort.Search(len(b.items), func(i int) bool {
		return b.items[i].Seq > afterSeq
	})

	if idx >= len(b.items) {
		return out[:0], false
	}

	available := len(b.items) - idx
	take := available
	hasMore := false
	if available > limit {
		take = limit
		hasMore = true
	}
	out = out[:0]
	for j := 0; j < take; j++ {
		out = append(out, *b.items[idx+j])
	}
	return out, hasMore
}

// getByID / getBySeq run in ~15 ns under RLock. At that size the
// `defer b.mu.RUnlock()` overhead is a measurable fraction of the
// total cost (Go 1.14 inlines the defer but not for free on this
// size of function), so the unlock is inlined manually here. Pattern:
// take RLock → do the map lookup → unlock → branch on ok. Hot read
// paths only — keep `defer` on slower paths where the overhead is
// noise.
func (b *scopeBuffer) getByID(id string) (Item, bool) {
	b.mu.RLock()
	item, ok := b.byID[id]
	b.mu.RUnlock()
	if !ok {
		return Item{}, false
	}
	return *item, true
}

func (b *scopeBuffer) getBySeq(seq uint64) (Item, bool) {
	b.mu.RLock()
	item, ok := b.bySeq[seq]
	b.mu.RUnlock()
	if !ok {
		return Item{}, false
	}
	return *item, true
}
