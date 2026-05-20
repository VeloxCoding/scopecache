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
// skipping `offset` from the newest end. The window is the slice
// b.items[start:end] preserved in its native seq-ascending
// (oldest-first) order; clients sort by seq if they want
// newest-first.
//
// hasMore is true when older items exist before the window (i.e.
// start > 0), signalling to the caller that the response is clipped
// at the oldest end. It does NOT signal truncation at the newest end
// (that is what offset already describes to the client).
func (b *scopeBuffer) tailOffset(limit int, offset int) ([]Item, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	if limit <= 0 || offset < 0 {
		return []Item{}, false
	}
	if offset >= len(b.items) {
		return []Item{}, false
	}

	end := len(b.items) - offset
	start := end - limit
	hasMore := start > 0
	if start < 0 {
		start = 0
	}
	if start >= end {
		return []Item{}, false
	}

	// items is []*Item; deref-copy the window into a fresh value slice
	// so the read path never hands back the live pointers.
	window := b.items[start:end]
	out := make([]Item, len(window))
	for i, p := range window {
		out[i] = *p
	}
	return out, hasMore
}

// sinceSeq returns items with seq > afterSeq, oldest-first, up to
// limit. limit ≤ 0 returns an empty slice — matches every other
// multi-item read on the public surface. HTTP rejects 0/negative
// with 400 via normalizeLimit; the guard here exists for direct
// Gateway callers.
//
// The bool is true when more matching items exist beyond the
// returned slice, so the handler surfaces truncated=true without
// the client guessing from count == limit.
func (b *scopeBuffer) sinceSeq(afterSeq uint64, limit int) ([]Item, bool) {
	if limit <= 0 {
		return []Item{}, false
	}

	b.mu.RLock()
	defer b.mu.RUnlock()

	if len(b.items) == 0 {
		return []Item{}, false
	}

	idx := sort.Search(len(b.items), func(i int) bool {
		return b.items[i].Seq > afterSeq
	})

	if idx >= len(b.items) {
		return []Item{}, false
	}

	available := len(b.items) - idx
	take := available
	hasMore := false
	if available > limit {
		take = limit
		hasMore = true
	}
	out := make([]Item, take)
	for j := 0; j < take; j++ {
		out[j] = *b.items[idx+j]
	}
	return out, hasMore
}

func (b *scopeBuffer) getByID(id string) (Item, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	item, ok := b.byID[id]
	if !ok {
		return Item{}, false
	}
	return *item, true
}

func (b *scopeBuffer) getBySeq(seq uint64) (Item, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	item, ok := b.bySeq[seq]
	if !ok {
		return Item{}, false
	}
	return *item, true
}
