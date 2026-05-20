// Locking invariants for *scopeBuffer
// -----------------------------------
//
//  1. Lock-acquisition order is strictly TOP-DOWN:
//     Store-level code may acquire scopeShard.mu BEFORE buf.mu.
//     Multi-shard ops additionally acquire shard.mu in ascending
//     shard-index order (see numShards comment in store.go).
//
//  2. scopeBuffer methods MUST NOT reach back up to acquire any
//     Store-level lock — neither scopeShard.mu nor any future
//     Store-side mutex — while holding b.mu. The only Store-state a
//     scopeBuffer method may touch with b.mu held is the atomic
//     counter (b.store.totalBytes.Add / b.store.reserveBytes); those
//     take no locks. Reverse-direction locking (buf → shard) would
//     deadlock against deleteScope, replaceScopes, wipe and
//     rebuildAll, all of which take shard.mu first and then
//     individual buf.mu's.
//
//  3. Read-path bookkeeping (recordRead) runs without taking b.mu —
//     it bumps the readCountTotal and lastAccessTS atomics directly.
//     This is what lets concurrent readers hold b.mu.RLock simultaneously
//     without serialising on the read counters. See recordRead in
//     buffer_heat.go.
//
//  4. items, byID and bySeq hold *Item, and all three indexes alias
//     the SAME *Item per entry. A method may mutate an item's fields
//     in place through any one index under b.mu.Lock — the other two
//     observe it with no re-sync. But a method MUST NOT hand a raw
//     *Item to a caller or retain one past the unlock: read paths
//     deref-copy into a value Item before returning. A leaked *Item
//     would let a caller mutate cache state outside b.mu.
//
// Adding a new scopeBuffer method that violates rule 2 is the most
// likely future deadlock — flag it in code review.
//
// File layout for scopeBuffer methods:
//
//	buffer.go          — struct + ctor + this invariant header
//	buffer_locked.go   — cross-cutting helpers (precomputeRenderBytes,
//	                     indexBySeqLocked, replaceItemAtIndexLocked,
//	                     reservePayloadDeltaLocked)
//	buffer_heat.go     — lock-free recordRead bookkeeping
//	buffer_write.go    — appendItem, upsertByID, updateByID, updateBySeq
//	buffer_delete.go   — deleteByID, deleteBySeq, deleteUpToSeq, deleteIndexLocked
//	buffer_replace.go  — scopeReplacement type, build / commit pipeline, replaceAll
//	buffer_read.go     — tailOffset, sinceSeq, getByID, getBySeq
//	buffer_stats.go    — approxSizeBytes, scopeStats type, stats()

package scopecache

import (
	"sync"
	"sync/atomic"
)

type scopeBuffer struct {
	mu sync.RWMutex
	// store is always non-nil: production buffers are built by
	// s.newscopeBuffer() and bind to their owning store, test buffers
	// go through newTestBuffer which wires a permissive private store
	// internally. The per-scope item-count cap and per-item byte cap
	// both read from b.store directly so admission rules cannot drift
	// between a per-scope cache and the store-level source of truth.
	store *store
	// detached is set true when the buffer has been unlinked from its Store
	// by /delete_scope, /wipe or /rebuild. Writes that reach a detached
	// buffer via a stale pointer return *ScopeDetachedError so the caller
	// learns the write did not take effect, rather than silently writing
	// into an orphan buffer that is unreachable and about to be GC'd.
	detached bool
	// items, byID and bySeq are pointer collections: every entry for a
	// given item is the SAME *Item. An in-place field write through any
	// one index is visible through the other two — no re-sync needed.
	// The insert paths (insertNewItemLocked, buildReplacementState)
	// must therefore store one shared pointer into all three; read
	// paths must deref-copy before handing an item out so a caller
	// never holds the live *Item.
	items   []*Item
	byID    map[string]*Item
	bySeq   map[uint64]*Item
	lastSeq uint64
	// bytes is the running sum of approxItemSize(item) over items. Only
	// mutated under b.mu; the store-level total is kept in sync via
	// s.reserveBytes (single-item write paths) and
	// scopeBuffer.commitReplacement (bulk /warm and /rebuild).
	bytes     int64
	createdTS int64
	// lastWriteTS is the microsecond timestamp of the most recent
	// write that touched this scope. Set under b.mu by every write
	// path; read under b.mu.RLock by stats(). Initialised equal to
	// createdTS so a freshly-created scope reports a non-zero value
	// (creation is the first "touch"). Surfaced as last_write_ts on
	// /stats; distinct from lastAccessTS, which tracks reads.
	lastWriteTS int64
	// lastAccessTS and readCountTotal are atomic so the read-hot path
	// (recordRead) stays lock-free. Taking b.mu here would serialise
	// every /get, /render, /head and /tail hit against a single
	// scope's mutex — the dominant lock-wait source on read-heavy
	// workloads. Keep it atomic.
	lastAccessTS   atomic.Int64
	readCountTotal atomic.Uint64
}
