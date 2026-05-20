// store.go owns the Store coordinator: a 32-shard scope map with
// admission control on a single store-wide byte budget. Every public
// HTTP/Gateway path lands here before reaching a *scopeBuffer.
//
// Cross-cutting invariants other files inherit:
//
//   - **Sharding.** scopes is split across numShards (32, power of 2)
//     independently-locked buckets. Single-scope ops take one shard
//     RLock; multi-scope ops (/wipe, /rebuild, /warm) MUST acquire
//     shard locks in ascending shard-index order to avoid deadlock.
//     Use lockAllShards / lockShards rather than ad-hoc loops.
//   - **Lockstep counters.** totalBytes / totalItems / scopeCount are
//     atomic counters maintained incrementally by every write path so
//     /stats stays O(1). Adding a new write/delete path means adding
//     the matching Adds in lockstep with the per-scope mutation —
//     drift corrupts /stats silently.
//   - **lastWriteTS.** Store-wide freshness signal, advanced via
//     bumpLastWriteTS (CAS-max). User-write success bumps it; counter
//     activity is silent (see buffer_counter.go), and pre-create /
//     rollback paths must NOT bump or polling clients see ghost ticks.
//   - **Byte budget.** reserveBytes is the only path that may push
//     totalBytes upward. Positive deltas use a CAS loop with overflow-
//     safe arithmetic; negative deltas (releases) always succeed. A
//     write that fails the cap leaves no state behind — caller returns
//     *StoreFullError without rollback.

package scopecache

import (
	"errors"
	"hash/maphash"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// numShards splits the scope map into independently-locked shards.
// Power of 2 so the modulo collapses to a bitmask. Multi-shard
// lock-order rule lives in the file header.
const (
	numShards = 32
	shardMask = numShards - 1
)

type scopeShard struct {
	mu     sync.RWMutex
	scopes map[string]*scopeBuffer
}

type store struct {
	// shards splits the scope map into numShards independently-locked
	// buckets. Per-scope hot paths take one shard's lock; multi-shard
	// ops take an ascending subset (see lockAllShards / lockShards).
	shards   [numShards]scopeShard
	hashSeed maphash.Seed

	defaultMaxItems int
	maxStoreBytes   int64
	maxItemBytes    int64

	// totalBytes is the authoritative byte counter for admission
	// control: Σ approxItemSize over items + scopeBufferOverhead per
	// allocated scope. Surfaced as approx_store_mb on /stats; the
	// 507-comparand, the /stats value, and the next-write check all
	// describe the same accounting model.
	//
	// Per-scope Go map/slice overhead is intentionally OUTSIDE this
	// counter — admission-control arithmetic stays layout-independent.
	// scopeBufferOverhead is the one per-scope cost that IS charged
	// (closes the empty-scope-spam DoS). Trade-off: approx_store_mb
	// under-reports real memory pressure at very high scope counts.
	totalBytes atomic.Int64

	// totalItems and scopeCount mirror the totalBytes lockstep
	// pattern: every write path that creates or removes an item /
	// scope adjusts the matching atomic alongside its per-scope
	// mutation. /stats reads them with three atomic loads, no shard
	// walks.
	//
	// Lockstep paths to maintain:
	//   - item insert: insertNewItemLocked + counterAdd create
	//     branch — totalItems.Add(+1) under b.mu.
	//   - item delete: deleteIndexLocked Add(-1); deleteUpToSeq
	//     Add(-N) for the batch.
	//   - bulk replace: commitReplacement(PreReserved) computes
	//     itemDelta = len(r.items) - len(b.items) at commit; the
	//     fresh len() reads handle stale-pointer drift implicitly.
	//   - scope create: getOrCreateScopeTrackingCreated and
	//     ensureScope Add(+1) inside the shard-write-lock section.
	//   - scope delete: cleanupIfEmptyAndUnused and deleteScope
	//     Add(-1) / Add(-itemCount) inside their shard-lock section.
	//   - bulk reset: wipe Store(0); rebuildAll Store(N) under
	//     all-shard write lock.
	//
	// Forgetting any of these corrupts /stats silently — same class
	// of bug as forgetting a totalBytes update. The invariant test
	// TestStore_StatsCounters_Invariant walks every shard and
	// asserts totalItems == Σ len(buf.items) and scopeCount ==
	// Σ len(sh.scopes); run it after touching any of these paths.
	totalItems atomic.Int64
	scopeCount atomic.Int64

	// lastWriteTS is the freshness signal surfaced on /stats: one
	// number a polling client compares against its previous value to
	// skip a refetch. Updated via bumpLastWriteTS (CAS-max) from
	// every per-scope write that bumps b.lastWriteTS, plus
	// store-level destructive paths (deleteScope, wipe, rebuildAll).
	//
	// CAS-max (rather than naive Store) is required because writes
	// from different scopes can compute time.Now().UnixMicro() out
	// of order across CPUs; a naive store could leave the counter at
	// the older value and lie about freshness.
	//
	// Default value is 0 (epoch start), distinguishable from any
	// real timestamp the cache would produce. /wipe immediately
	// bumps to its own commit time so a client polling right after
	// a wipe still sees a non-zero "something happened" tick.
	lastWriteTS atomic.Int64
}

// bumpLastWriteTS advances s.lastWriteTS to nowUs if nowUs > current.
// CAS-max (not Store) so out-of-order timestamps from different CPUs
// cannot regress the freshness signal.
//
// Loop terminates in 1 iteration when uncontested. No lock taken —
// safe to call from inside b.mu critical sections.
func (s *store) bumpLastWriteTS(nowUs int64) {
	for {
		cur := s.lastWriteTS.Load()
		if nowUs <= cur {
			return
		}
		if s.lastWriteTS.CompareAndSwap(cur, nowUs) {
			return
		}
	}
}

func newStore(c Config) *store {
	c = c.WithDefaults()
	s := &store{
		hashSeed:        maphash.MakeSeed(),
		defaultMaxItems: c.ScopeMaxItems,
		maxStoreBytes:   c.MaxStoreBytes,
		maxItemBytes:    c.MaxItemBytes,
	}
	for i := range s.shards {
		s.shards[i].scopes = make(map[string]*scopeBuffer)
	}
	return s
}

// shardIdxFor maps a scope name to a shard index in [0, numShards).
// maphash uses a per-process random seed, so distribution is uniform
// across shards and not predictable from the scope name — adversarial
// scope-name picking cannot deliberately collide on one shard.
func (s *store) shardIdxFor(scope string) uint64 {
	return maphash.String(s.hashSeed, scope) & shardMask
}

func (s *store) shardFor(scope string) *scopeShard {
	return &s.shards[s.shardIdxFor(scope)]
}

// shardsForScopes returns the unique set of shards covering the given
// scope names, in ascending shard-index order. Used by /warm to lock
// only the shards it touches (rather than all numShards) while still
// preserving the "all relevant shards held simultaneously" invariant
// that serialises against /wipe and /rebuild.
//
// `seen` is indexed by shard-index, so iterating it in order produces
// the ascending sequence directly — no sort, no intermediate index
// slice.
func (s *store) shardsForScopes(scopes []string) []*scopeShard {
	var seen [numShards]bool
	for _, scope := range scopes {
		seen[s.shardIdxFor(scope)] = true
	}
	out := make([]*scopeShard, 0, numShards)
	for i := 0; i < numShards; i++ {
		if seen[i] {
			out = append(out, &s.shards[i])
		}
	}
	return out
}

// lockAllShards / unlockAllShards / lockShards / unlockShards are
// the helpers every multi-shard mutation MUST use. Acquisition is
// ascending shard-index order (see file header); release order is
// not load-bearing.

// lockAllShards locks every shard in ascending index order. Used by
// /wipe and /rebuild.
func (s *store) lockAllShards() {
	for i := range s.shards {
		s.shards[i].mu.Lock()
	}
}

// unlockAllShards is the matching release for lockAllShards.
func (s *store) unlockAllShards() {
	for i := range s.shards {
		s.shards[i].mu.Unlock()
	}
}

// lockShards locks the given subset. The slice MUST already be in
// ascending shard-index order — `shardsForScopes` returns it that way.
// Used by /warm so it only blocks the shards its batch touches.
func lockShards(shards []*scopeShard) {
	for _, sh := range shards {
		sh.mu.Lock()
	}
}

// unlockShards is the matching release for lockShards.
func unlockShards(shards []*scopeShard) {
	for _, sh := range shards {
		sh.mu.Unlock()
	}
}

// reserveBytes atomically adjusts the store byte counter by delta, enforcing
// the cap for positive deltas. Negative deltas (releases) always succeed.
// Returns (ok, totalAfterAttempt, cap). Positive deltas use a CAS loop so
// concurrent /append writers never collectively over-commit the cap.
//
// The cap-check is written as `delta > maxStoreBytes-current` rather than
// `current+delta > maxStoreBytes` to avoid signed-int64 overflow when an
// operator configures MaxStoreBytes near math.MaxInt64. The naive form
// fails OPEN: current+delta wraps to negative, the `> maxStoreBytes`
// comparison passes, and a write that violates the cap is admitted.
// The subtractive form is algebraically equivalent in the safe range and
// stays correct at the boundary because maxStoreBytes-current can only
// underflow if current is negative, which our admission control prevents
// for positive-delta callers.
func (s *store) reserveBytes(delta int64) (bool, int64, int64) {
	if delta <= 0 {
		n := s.totalBytes.Add(delta)
		return true, n, s.maxStoreBytes
	}
	for {
		current := s.totalBytes.Load()
		if delta > s.maxStoreBytes-current {
			return false, current, s.maxStoreBytes
		}
		next := current + delta
		if s.totalBytes.CompareAndSwap(current, next) {
			return true, next, s.maxStoreBytes
		}
	}
}

// addClampedInt64 returns a + b, saturating at math.MaxInt64 / math.MinInt64
// instead of wrapping. Used to derive caps (eventsMaxItemBytes) from operator
// inputs that could be near the int64 boundary without making the derived cap
// silently negative. Callers stay correct under any pathological config.
func addClampedInt64(a, b int64) int64 {
	if b > 0 && a > math.MaxInt64-b {
		return math.MaxInt64
	}
	if b < 0 && a < math.MinInt64-b {
		return math.MinInt64
	}
	return a + b
}

// scopeBufferOverhead is the byte-cost the cache charges per allocated
// scope, on top of the scope's items. Covers the *scopeBuffer struct
// itself (mutex, slice header, two map headers, heat-bucket
// ringbuffer, scope-name string in its shard's map), plus slack for the
// per-key map entry overhead. A conservative single-KiB number.
//
// Including it in totalBytes admission control means a workload that
// spams empty scopes — whether by accident or as part of a misbehaving
// addon — hits the store-byte cap (default 100 MiB ≈ 100k empty
// scopes) and 507's instead of growing memory unbounded. Without
// this, totalBytes only counts payload bytes — 1M empty scopes
// consume ~1 GiB of struct memory but report approx_store_mb = 0.
//
// This is also a /stats accuracy improvement: approx_store_mb now
// matches actual memory pressure, not just item bytes.
const scopeBufferOverhead = 1024

// newscopeBuffer builds a fresh scopeBuffer bound to this store so its
// mutations can participate in byte tracking. Keeping this helper on the
// store means every production path creates bound buffers; tests that
// exercise scopeBuffer in isolation use newscopeBuffer directly and
// accept that byte tracking is a no-op there.
func (s *store) newscopeBuffer() *scopeBuffer {
	b := newscopeBuffer(s.defaultMaxItems)
	b.store = s
	b.maxItemBytes = s.maxItemBytes
	return b
}

func (s *store) getOrCreateScope(scope string) (*scopeBuffer, error) {
	buf, _, err := s.getOrCreateScopeTrackingCreated(scope)
	return buf, err
}

// getOrCreateScopeTrackingCreated is the variant used by the atomic
// write paths (appendOne, upsertOne, counterAddOne) that need to know
// whether the buffer was freshly allocated by this call. Callers use
// the `created` flag to roll the empty scope back when the subsequent
// item-byte reservation fails — see cleanupIfEmptyAndUnused. All other
// callers go through getOrCreateScope, which discards the flag.
func (s *store) getOrCreateScopeTrackingCreated(scope string) (*scopeBuffer, bool, error) {
	if scope == "" {
		return nil, false, errors.New("the 'scope' field is required")
	}

	sh := s.shardFor(scope)

	sh.mu.RLock()
	buf, ok := sh.scopes[scope]
	sh.mu.RUnlock()
	if ok {
		return buf, false, nil
	}

	// Allocate the buffer BEFORE taking the shard write lock — the
	// expensive part (struct init, map slot reservation, GC pressure
	// at high create rates) happens while other goroutines can still
	// progress on this shard. The byte reservation stays INSIDE the
	// lock so /wipe's totalBytes.Swap(0) and /rebuild's
	// totalBytes.Store() (both held under all shard locks) cannot
	// race with our reservation: while we hold this shard's lock,
	// neither op can run, and our reservation is observed atomically
	// alongside the map insert.
	//
	// Race-loss path (a concurrent goroutine inserted the same scope
	// while we were allocating) discards the unused buffer for GC and
	// makes no reservation. Race-loss is rare in unique-scope-per-
	// write workloads (every scope name distinct); same-scope writes
	// hit the RLock fast-path above instead.
	preBuf := s.newscopeBuffer()

	sh.mu.Lock()
	if existing, ok := sh.scopes[scope]; ok {
		sh.mu.Unlock()
		return existing, false, nil
	}
	if ok, current, max := s.reserveBytes(scopeBufferOverhead); !ok {
		sh.mu.Unlock()
		return nil, false, &StoreFullError{
			StoreBytes: current,
			AddedBytes: scopeBufferOverhead,
			Cap:        max,
		}
	}
	sh.scopes[scope] = preBuf
	s.scopeCount.Add(1)
	// NO bumpLastWriteTS here. This scope is a precursor to the
	// caller's item write — its success path emits its own bump.
	// s.lastWriteTS is CAS-max-monotonic, so a precursor bump cannot
	// be undone if the item reservation fails and
	// cleanupIfEmptyAndUnused rolls the scope back. Polling clients
	// would then see a tick that corresponds to no committed change.
	//
	// Empty-scope-as-goal creates (infrastructure scopes) go through
	// ensureScope, which bumps unconditionally — there the scope's
	// existence IS the committed state change.
	sh.mu.Unlock()
	return preBuf, true, nil
}

// cleanupIfEmptyAndUnused rolls back a freshly-created scope when the
// caller's subsequent item-byte reservation failed. Without this, every
// failed write to a new scope would leak scopeBufferOverhead bytes onto
// the store-byte cap, which a multi-tenant attacker could exploit to
// fill the cap with empty scopes (DoS).
//
// Three guards prevent collateral damage:
//   - cur == buf: another caller may have wiped+recreated the scope
//     between our create and our cleanup; only delete if our buffer is
//     still the one mapped at this name.
//   - len(buf.items) == 0: a concurrent writer that grabbed our buf
//     pointer through the fast path may have committed an item before
//     we acquired buf.mu; if so, the scope is no longer "empty" and we
//     must leave it alone.
//   - detached + store=nil: matches deleteScope's pattern. Any
//     concurrent in-flight writer that wakes up on this buf after we
//     released the locks returns *ScopeDetachedError, same semantics
//     as a /delete_scope race.
func (s *store) cleanupIfEmptyAndUnused(scope string, buf *scopeBuffer) {
	sh := s.shardFor(scope)
	sh.mu.Lock()
	defer sh.mu.Unlock()

	cur, ok := sh.scopes[scope]
	if !ok || cur != buf {
		return
	}

	buf.mu.Lock()
	defer buf.mu.Unlock()

	if len(buf.items) != 0 {
		return
	}

	delete(sh.scopes, scope)
	s.totalBytes.Add(-scopeBufferOverhead)
	s.scopeCount.Add(-1)
	buf.detached = true
	buf.store = nil
}

// appendOne is the atomic /append write-path for external callers
// (HTTP handlers + Gateway.Append). Validates the item, then routes
// through appendOneTrusted for the actual commit.
func (s *store) appendOne(item Item) (Item, error) {
	if err := validateWriteItem(&item, "/append", s.maxItemBytes); err != nil {
		return Item{}, err
	}
	return s.appendOneTrusted(item)
}

// appendOneTrusted is the post-validation commit path used by
// appendOne after shape validation. Skips the validator entirely;
// callers are responsible for delivering an item the validator would
// have accepted.
func (s *store) appendOneTrusted(item Item) (Item, error) {
	buf, created, err := s.getOrCreateScopeTrackingCreated(item.Scope)
	if err != nil {
		return Item{}, err
	}
	result, appendErr := buf.appendItem(item)
	if appendErr != nil {
		if created {
			s.cleanupIfEmptyAndUnused(item.Scope, buf)
		}
		return result, appendErr
	}
	return result, nil
}

// upsertOne is the atomic /upsert write-path; same rollback contract
// as appendOne. Returns (item, created, err) where created reflects
// the upsert outcome, not the scope-creation outcome.
func (s *store) upsertOne(item Item) (Item, bool, error) {
	if err := validateUpsertItem(&item, s.maxItemBytes); err != nil {
		return Item{}, false, err
	}
	buf, scopeCreated, err := s.getOrCreateScopeTrackingCreated(item.Scope)
	if err != nil {
		return Item{}, false, err
	}
	result, itemCreated, upsertErr := buf.upsertByID(item)
	if upsertErr != nil {
		if scopeCreated {
			s.cleanupIfEmptyAndUnused(item.Scope, buf)
		}
		return result, itemCreated, upsertErr
	}
	return result, itemCreated, nil
}

// counterAddOne is the atomic /counter_add write-path; same rollback
// contract as appendOne. Returns (value, created, err) where created
// reflects the counter outcome, not the scope-creation outcome.
func (s *store) counterAddOne(scope, id string, by int64) (int64, bool, error) {
	// Construct the request shape the validator expects so the same
	// rules (scope shape + by != 0 + range) apply to both HTTP and
	// Go-API callers. The pointer-nil check (`by required`) is the
	// HTTP-only concern (JSON missing-field detection) and stays in
	// the handler — Go callers always pass an int64.
	if _, err := validateCounterAddRequest(counterAddRequest{Scope: scope, ID: id, By: &by}, s.maxItemBytes); err != nil {
		return 0, false, err
	}
	buf, scopeCreated, err := s.getOrCreateScopeTrackingCreated(scope)
	if err != nil {
		return 0, false, err
	}
	value, counterCreated, addErr := buf.counterAdd(scope, id, by)
	if addErr != nil {
		if scopeCreated {
			s.cleanupIfEmptyAndUnused(scope, buf)
		}
		return value, counterCreated, addErr
	}
	// Counter mutations are silent on s.lastWriteTS by design — but
	// when counter_add allocated a brand-new scope as a side effect,
	// scope_count just grew on /stats and polling clients need a
	// tick. Bump after the commit (not in getOrCreate) so a cell-
	// allocation failure doesn't leave a ghost tick.
	if scopeCreated {
		s.bumpLastWriteTS(nowUnixMicro())
	}
	return value, counterCreated, nil
}

// updateOne mutates the payload of an item addressed by scope+id or
// scope+seq. Returns (updated_count, err); a missing scope is reported
// as (0, nil), the same wire shape an absent id/seq inside an existing
// scope would produce. The caller-side validator enforces the
// id-xor-seq invariant; updateOne assumes id != "" picks the id path
// and otherwise routes by seq.
//
// Emits an update event into `_events` ONLY on hit (updated > 0): a
// miss is a no-op against cache state, so emitting it would be result-
// logging (the request) rather than action-logging (the change).
// Drainers replaying `_events` against an empty cache produce the
// same final state without these noise entries.
func (s *store) updateOne(item Item) (int, error) {
	if err := validateUpdateItem(&item, s.maxItemBytes); err != nil {
		return 0, err
	}
	buf, ok := s.getScope(item.Scope)
	if !ok {
		return 0, nil
	}
	var (
		updated int
		err     error
	)
	if item.ID != "" {
		updated, err = buf.updateByID(item.ID, item.Payload, item.renderBytes)
	} else {
		updated, err = buf.updateBySeq(item.Seq, item.Payload, item.renderBytes)
	}
	if err != nil {
		return updated, err
	}
	return updated, nil
}

// deleteOne removes a single item by scope+id or scope+seq. Returns
// (deleted_count, err); missing scope reports (0, nil) — same miss
// shape as updateOne. Validator-enforced id-xor-seq invariant.
func (s *store) deleteOne(scope, id string, seq uint64) (int, error) {
	if err := validateDeleteRequest(deleteRequest{Scope: scope, ID: id, Seq: seq}); err != nil {
		return 0, err
	}
	buf, ok := s.getScope(scope)
	if !ok {
		return 0, nil
	}
	var (
		deleted int
		err     error
	)
	if id != "" {
		deleted, err = buf.deleteByID(id)
	} else {
		deleted, err = buf.deleteBySeq(seq)
	}
	if err != nil {
		return deleted, err
	}
	return deleted, nil
}

// deleteUpTo removes every item in the scope with seq <= maxSeq.
// Returns (deleted_count, err); missing scope reports (0, nil).
func (s *store) deleteUpTo(scope string, maxSeq uint64) (int, error) {
	if err := validateDeleteUpToRequest(deleteUpToRequest{Scope: scope, MaxSeq: maxSeq}); err != nil {
		return 0, err
	}
	buf, ok := s.getScope(scope)
	if !ok {
		return 0, nil
	}
	deleted, err := buf.deleteUpToSeq(maxSeq)
	if err != nil {
		return deleted, err
	}
	return deleted, nil
}

// head returns up to `limit` oldest items in the scope with seq >
// afterSeq. Returns (items, truncated, scopeFound). On a non-empty
// result the scope's read-bookkeeping atomics are bumped via
// recordRead. A missing scope reports (nil, false, false); a
// found-but-empty window reports (empty, false, true) — handlers
// translate that into hit:false / count:0.
func (s *store) head(scope string, afterSeq uint64, limit int) ([]Item, bool, bool) {
	buf, ok := s.getScope(scope)
	if !ok {
		return nil, false, false
	}
	items, truncated := buf.sinceSeq(afterSeq, limit)
	if len(items) > 0 {
		buf.recordRead(nowUnixMicro())
	}
	return items, truncated, true
}

// tail returns up to `limit` newest items in the scope, skipping the
// first `offset`. Same return shape and bookkeeping as head.
func (s *store) tail(scope string, limit, offset int) ([]Item, bool, bool) {
	buf, ok := s.getScope(scope)
	if !ok {
		return nil, false, false
	}
	items, truncated := buf.tailOffset(limit, offset)
	if len(items) > 0 {
		buf.recordRead(nowUnixMicro())
	}
	return items, truncated, true
}

// get returns one item by scope+id or scope+seq. (item, found) — the
// found flag is true only when both the scope and the item exist.
// recordRead fires on hit only.
func (s *store) get(scope, id string, seq uint64) (Item, bool) {
	buf, ok := s.getScope(scope)
	if !ok {
		return Item{}, false
	}
	var item Item
	var found bool
	if id != "" {
		item, found = buf.getByID(id)
	} else {
		item, found = buf.getBySeq(seq)
	}
	if !found {
		return Item{}, false
	}
	buf.recordRead(nowUnixMicro())
	return item, true
}

// render returns the bytes /render writes on the wire, peeling the
// renderBytes shortcut for JSON-string payloads at the Store boundary
// so the handler does not need to know the renderBytes field exists.
// (bytes, found) — same hit semantics as get; recordRead fires on hit.
func (s *store) render(scope, id string, seq uint64) ([]byte, bool) {
	buf, ok := s.getScope(scope)
	if !ok {
		return nil, false
	}
	var item Item
	var found bool
	if id != "" {
		item, found = buf.getByID(id)
	} else {
		item, found = buf.getBySeq(seq)
	}
	if !found {
		return nil, false
	}
	buf.recordRead(nowUnixMicro())
	if item.renderBytes != nil {
		return item.renderBytes, true
	}
	return item.Payload, true
}

// ensureScope returns the named scope, creating an empty buffer if it
// does not yet exist. Used by API-layer features that lazily provision
// cache-owned infrastructure scopes (e.g. observability counters)
// without requiring operator pre-provisioning. Idempotent — safe to
// call on every request; cost is one map lookup under the read-lock
// when the scope already exists.
//
// Unlike getOrCreateScope, this method does not validate the scope
// name and is intended only for cache-internal infrastructure scopes
// whose names are compile-time constants on the caller side.
//
// Reserves scopeBufferOverhead just like getOrCreateScope on the
// create path. This is required for accounting symmetry: deleteScope
// unconditionally subtracts (scopeBytes + scopeBufferOverhead), so a
// later admin-driven delete of these scopes would otherwise drift
// totalBytes scopeBufferOverhead bytes too low per cycle (potentially
// negative) — the bytes-counter invariant is "totalBytes == sum of
// reservations". Returns nil when the cap is exhausted; callers are
// expected to treat such infrastructure writes as best-effort and
// silently skip on nil (observability is not auth — losing a counter
// must never block a legitimate request).
func (s *store) ensureScope(scope string) *scopeBuffer {
	sh := s.shardFor(scope)

	sh.mu.RLock()
	buf, ok := sh.scopes[scope]
	sh.mu.RUnlock()
	if ok {
		return buf
	}

	sh.mu.Lock()
	defer sh.mu.Unlock()

	buf, ok = sh.scopes[scope]
	if ok {
		return buf
	}

	if ok, _, _ := s.reserveBytes(scopeBufferOverhead); !ok {
		return nil
	}

	buf = s.newscopeBuffer()
	sh.scopes[scope] = buf
	s.scopeCount.Add(1)
	// scope_count just changed, so the freshness tick advances.
	s.bumpLastWriteTS(buf.lastWriteTS)
	return buf
}

func (s *store) getScope(scope string) (*scopeBuffer, bool) {
	sh := s.shardFor(scope)
	sh.mu.RLock()
	defer sh.mu.RUnlock()

	buf, ok := sh.scopes[scope]
	return buf, ok
}

func (s *store) deleteScope(scope string) (int, bool, error) {
	// validateDeleteScopeRequest enforces the same shape rules as
	// /delete_scope (scope present + non-reserved). Empty scope
	// triggers the validator's "scope required" reject; reserved scope
	// triggers the "is reserved" reject — both come back wrapped in
	// ErrInvalidInput so the handler can map to 400.
	if err := validateDeleteScopeRequest(deleteScopeRequest{Scope: scope}); err != nil {
		return 0, false, err
	}

	sh := s.shardFor(scope)
	sh.mu.Lock()
	defer sh.mu.Unlock()

	buf, ok := sh.scopes[scope]
	if !ok {
		return 0, false, nil
	}

	// Hold buf.mu as a write lock across the whole sequence so an in-flight
	// mutator on this buf (via a stale pointer obtained before we ran) either
	// completes before we touch the counter or waits until after we're done.
	// Crucially we also detach the buffer: any write that wakes up afterwards
	// returns *ScopeDetachedError instead of silently writing into an orphan
	// that is unreachable and about to be GC'd. store is cleared too so any
	// remaining code path that survives the detach check still skips
	// store-counter accounting.
	buf.mu.Lock()
	itemCount := len(buf.items)
	scopeBytes := buf.bytes
	delete(sh.scopes, scope)
	// Release item bytes AND the per-scope overhead reserved at create
	// time. Combined into one Add so observers never see a transient
	// state with one released and the other still charged.
	s.totalBytes.Add(-(scopeBytes + scopeBufferOverhead))
	s.totalItems.Add(-int64(itemCount))
	s.scopeCount.Add(-1)
	s.bumpLastWriteTS(nowUnixMicro())
	buf.detached = true
	buf.store = nil
	buf.mu.Unlock()
	return itemCount, true, nil
}

// storeStats is the typed snapshot of the store. stats() returns it so the
// /stats handler can flatten it into orderedFields for the wire, and so any
// in-package caller (tests, future adapters) can read fields directly.
//
// The aggregate fields are intentionally not a per-scope map: at
// 100k+ scopes, per-scope enumeration was the dominant cost of /stats
// and routinely blew past practical client/proxy response limits.
// Per-scope enumeration lives at /scopelist, which paginates
// alphabetically.
type storeStats struct {
	Scopes        int   `json:"scopes"`
	Items         int   `json:"items"`
	ApproxStoreMB MB    `json:"approx_store_mb"`
	LastWriteTS   int64 `json:"last_write_ts"`
}

// stats returns the store-wide aggregate snapshot in O(1) — four
// atomic loads, no shard walks. Configured caps (MaxStoreBytes, etc.)
// are NOT echoed here; they are static config and belong on /help.
//
// Each load is independent, so a concurrent burst of /append +
// /delete can produce a snapshot where (scope_count, total_items,
// approx_store_mb) reflect three slightly different instants. /stats
// is an advisory snapshot.
func (s *store) stats() storeStats {
	return storeStats{
		Scopes:        int(s.scopeCount.Load()),
		Items:         int(s.totalItems.Load()),
		ApproxStoreMB: MB(s.totalBytes.Load()),
		LastWriteTS:   s.lastWriteTS.Load(),
	}
}

func (s *store) listScopes() map[string]*scopeBuffer {
	out := make(map[string]*scopeBuffer)
	for i := range s.shards {
		sh := &s.shards[i]
		sh.mu.RLock()
		for k, v := range sh.scopes {
			out[k] = v
		}
		sh.mu.RUnlock()
	}
	return out
}

// scopeListEntry is one row in a /scopelist response: the scope name plus
// the seven per-scope primitives §2.4 of the RFC maintains directly.
// Field declaration order = wire field order (encoding/json honours it).
type scopeListEntry struct {
	Scope          string `json:"scope"`
	ItemCount      int    `json:"item_count"`
	LastSeq        uint64 `json:"last_seq"`
	ApproxScopeMB  MB     `json:"approx_scope_mb"`
	CreatedTS      int64  `json:"created_ts"`
	LastWriteTS    int64  `json:"last_write_ts"`
	LastAccessTS   int64  `json:"last_access_ts"`
	ReadCountTotal uint64 `json:"read_count_total"`
}

// scopeList returns the per-scope detail rows for /scopelist: optional
// prefix filter, alphabetical sort, cursor pagination by name. Returns
// (entries, truncated) where truncated is true when more matching scopes
// exist past the limit window.
//
// Cost shape:
//   - O(N) walk across every shard map under each shard's RLock; the
//     filter (prefix match + after-cursor) runs inside the loop so only
//     matching names enter the sort buffer.
//   - O(M log M) sort, where M = filtered count.
//   - O(limit) buf.stats() materialisations after the locks are released.
//
// Stats() takes its own buf.mu.RLock per scope, so a slow stats() call
// cannot block writers on its shard for the duration of the listing.
// A scope deleted between the snapshot and stats() materialises its
// last-known state — same advisory-snapshot caveat as /stats (§7.3).
func (s *store) scopeList(prefix, after string, limit int) ([]scopeListEntry, bool) {
	// Defensive guard: limit <= 0 returns empty without scanning the
	// shards. The HTTP path's normalizeLimit rejects 0/negative with
	// 400 before calling, but Gateway callers pass int through
	// untouched — without this guard a negative limit reaches
	// `refs[:limit]` below and triggers a slice-bounds panic.
	// Uniform across every multi-item read on the public surface
	// (tailOffset, sinceSeq, scopeList): "give me ≤ 0 items" is
	// answered with the empty result, not "give me everything" and
	// not a panic.
	if limit <= 0 {
		return []scopeListEntry{}, false
	}

	type ref struct {
		name string
		buf  *scopeBuffer
	}
	var refs []ref
	for i := range s.shards {
		sh := &s.shards[i]
		sh.mu.RLock()
		for name, buf := range sh.scopes {
			if prefix != "" && !strings.HasPrefix(name, prefix) {
				continue
			}
			if after != "" && name <= after {
				continue
			}
			refs = append(refs, ref{name: name, buf: buf})
		}
		sh.mu.RUnlock()
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].name < refs[j].name })

	truncated := len(refs) > limit
	if truncated {
		refs = refs[:limit]
	}

	out := make([]scopeListEntry, 0, len(refs))
	for _, r := range refs {
		st := r.buf.stats()
		out = append(out, scopeListEntry{
			Scope:          r.name,
			ItemCount:      st.ItemCount,
			LastSeq:        st.LastSeq,
			ApproxScopeMB:  st.ApproxScopeMB,
			CreatedTS:      st.CreatedTS,
			LastWriteTS:    st.LastWriteTS,
			LastAccessTS:   st.LastAccessTS,
			ReadCountTotal: st.ReadCountTotal,
		})
	}
	return out, truncated
}
