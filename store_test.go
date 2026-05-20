package scopecache

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sync"
	"testing"
)

// --- Store --------------------------------------------------------------------

func TestStore_GetOrCreateScope_RequiresScope(t *testing.T) {
	s := newStore(Config{ScopeMaxItems: 10, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})
	if _, err := s.getOrCreateScope(""); err == nil {
		t.Fatal("expected error for empty scope")
	}
}

func TestStore_GetOrCreateScope_ReturnsSameBuffer(t *testing.T) {
	s := newStore(Config{ScopeMaxItems: 10, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})
	b1, _ := s.getOrCreateScope("x")
	b2, _ := s.getOrCreateScope("x")
	if b1 != b2 {
		t.Fatal("scope buffers should be identical")
	}
}

// NewStore(Config{}) + NewAPI(s, APIConfig{}) must produce a usable
// Store + API. Pre-fix the zero Config carried zero caps to every field,
// so any positive write failed with StoreFullError or worse — the public
// package was effectively dead-on-arrival for library users.
func TestNewStore_ZeroConfigUsesDefaults(t *testing.T) {
	s := newStore(Config{})
	api := NewAPI(&Gateway{store: s}, APIConfig{})
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	// A normal /append must just work.
	body := `{"scope":"smoke","id":"a","payload":{"v":1}}`
	code, _, raw := doRequest(t, mux, "POST", "/append", body)
	if code != 200 {
		t.Fatalf("/append on default-config Store: code=%d body=%s", code, raw)
	}

	// Cache caps must match the package-level compile-time defaults.
	if s.defaultMaxItems != ScopeMaxItems {
		t.Errorf("defaultMaxItems=%d want %d", s.defaultMaxItems, ScopeMaxItems)
	}
	if s.maxStoreBytes != int64(MaxStoreMiB)<<20 {
		t.Errorf("maxStoreBytes=%d want %d", s.maxStoreBytes, int64(MaxStoreMiB)<<20)
	}
	if s.maxItemBytes != int64(MaxItemBytes) {
		t.Errorf("maxItemBytes=%d want %d", s.maxItemBytes, int64(MaxItemBytes))
	}
	// HTTP response cap is derived from the store's byte cap (no
	// separate APIConfig knob): a single scope can never exceed the
	// store budget, so the response cap that "guarantees every full-
	// scope tail fits in one response" is exactly the store cap.
	if api.maxResponseBytes != s.maxStoreBytes {
		t.Errorf("api.maxResponseBytes=%d want %d (= store.maxStoreBytes)", api.maxResponseBytes, s.maxStoreBytes)
	}
}

// NewAPI derives the response cap from the store's byte cap rather
// than from a separate APIConfig knob: any single scope is bounded by
// the store budget, so the response cap that's "guaranteed to fit
// every full-scope tail in one response" is simply equal to the store
// cap. Operators tune MaxStoreBytes; the response cap follows.
func TestNewAPI_DerivesMaxResponseBytesFromStore(t *testing.T) {
	t.Run("default store", func(t *testing.T) {
		s := newStore(Config{})
		api := NewAPI(&Gateway{store: s}, APIConfig{})
		if api.maxResponseBytes != s.maxStoreBytes {
			t.Errorf("maxResponseBytes=%d want %d (= store.maxStoreBytes)",
				api.maxResponseBytes, s.maxStoreBytes)
		}
	})

	t.Run("custom store cap propagates", func(t *testing.T) {
		s := newStore(Config{MaxStoreBytes: 7 << 20})
		api := NewAPI(&Gateway{store: s}, APIConfig{})
		if api.maxResponseBytes != int64(7<<20) {
			t.Errorf("maxResponseBytes=%d want %d", api.maxResponseBytes, 7<<20)
		}
	})
}

func TestStore_GetScope_Miss(t *testing.T) {
	s := newStore(Config{ScopeMaxItems: 10, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})
	if _, ok := s.getScope("nope"); ok {
		t.Fatal("expected miss")
	}
}

// updateOne, deleteOne, deleteUpTo all share a "missing scope = (0, nil)"
// contract that handlers translate into hit:false / count:0 wire shape.
// Pin it explicitly so a future refactor cannot quietly change miss
// semantics to (0, ScopeNotFoundError) — handlers would then surface 409
// for what should be a 200 miss response.

func TestStore_updateOne_MissingScope(t *testing.T) {
	s := newStore(Config{ScopeMaxItems: 10, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})
	item := Item{Scope: "nope", ID: "x", Payload: json.RawMessage(`"v"`)}
	n, err := s.updateOne(item)
	if err != nil {
		t.Fatalf("err=%v; want nil", err)
	}
	if n != 0 {
		t.Errorf("updated=%d; want 0", n)
	}
}

func TestStore_deleteOne_MissingScope(t *testing.T) {
	s := newStore(Config{ScopeMaxItems: 10, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})
	n, err := s.deleteOne("nope", "x", 0)
	if err != nil {
		t.Fatalf("err=%v; want nil", err)
	}
	if n != 0 {
		t.Errorf("deleted=%d; want 0", n)
	}
}

func TestStore_deleteUpTo_MissingScope(t *testing.T) {
	s := newStore(Config{ScopeMaxItems: 10, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})
	n, err := s.deleteUpTo("nope", 100)
	if err != nil {
		t.Fatalf("err=%v; want nil", err)
	}
	if n != 0 {
		t.Errorf("deleted=%d; want 0", n)
	}
}

// head, tail, get, render report a missing scope by setting their
// found-flag to false. handleHead/Tail use it to pick writeItemsMiss
// vs writeItemsHit; handleGet/Render use it to write the miss
// response. A future change that returned (nil, true, false) for
// missing scopes would silently break /head and /tail by routing
// misses through writeItemsHit (different wire shape).

func TestStore_head_MissingScope(t *testing.T) {
	s := newStore(Config{ScopeMaxItems: 10, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})
	items, truncated, found := s.head("nope", 0, 10, nil)
	if found {
		t.Error("found=true; want false")
	}
	if len(items) != 0 {
		t.Errorf("items=%v; want empty", items)
	}
	if truncated {
		t.Error("truncated=true; want false")
	}
}

func TestStore_tail_MissingScope(t *testing.T) {
	s := newStore(Config{ScopeMaxItems: 10, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})
	items, truncated, found := s.tail("nope", 10, 0, nil)
	if found {
		t.Error("found=true; want false")
	}
	if len(items) != 0 {
		t.Errorf("items=%v; want empty", items)
	}
	if truncated {
		t.Error("truncated=true; want false")
	}
}

func TestStore_get_MissingScope(t *testing.T) {
	s := newStore(Config{ScopeMaxItems: 10, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})
	if _, found := s.get("nope", "x", 0); found {
		t.Error("found=true; want false")
	}
}

func TestStore_render_MissingScope(t *testing.T) {
	s := newStore(Config{ScopeMaxItems: 10, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})
	if _, found := s.render("nope", "x", 0); found {
		t.Error("found=true; want false")
	}
}

// render peels the renderBytes shortcut for JSON-string payloads — a
// store-level invariant that handleRender used to enforce inline.
// Pin it on the Store boundary now that the handler is dumb.
func TestStore_render_PeelsJSONString(t *testing.T) {
	s := newStore(Config{ScopeMaxItems: 10, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})
	_, err := s.appendOne(Item{Scope: "s", ID: "html", Payload: json.RawMessage(`"<h1>hi</h1>"`)})
	if err != nil {
		t.Fatalf("appendOne: %v", err)
	}
	body, found := s.render("s", "html", 0)
	if !found {
		t.Fatal("found=false; want true")
	}
	if got := string(body); got != "<h1>hi</h1>" {
		t.Errorf("body=%q; want %q (renderBytes shortcut should peel JSON-string layer)", got, "<h1>hi</h1>")
	}
}

// appendOne and upsertOne must roll back the freshly-created scope
// when the item-byte reservation fails. Without rollback, every failed
// write to a new scope would leak scopeBufferOverhead onto the store-
// byte cap — a multi-tenant attacker could fill the cap with empty
// scopes and DoS legitimate writers.

// bigPayload returns a JSON string payload of approximately the given
// total byte count. Used by the rollback tests to push past the
// store-byte cap without hitting the per-item cap.
func bigPayload(n int) json.RawMessage {
	buf := make([]byte, n+2)
	buf[0] = '"'
	for i := 1; i < n+1; i++ {
		buf[i] = 'a'
	}
	buf[n+1] = '"'
	return json.RawMessage(buf)
}

func TestStore_EnsureScope_CreatesEmpty(t *testing.T) {
	s := newStore(Config{ScopeMaxItems: 10, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})
	buf := s.ensureScope("_counters_count_calls")
	if buf == nil {
		t.Fatal("ensureScope returned nil")
	}
	if got, ok := s.getScope("_counters_count_calls"); !ok || got != buf {
		t.Fatal("scope not registered or different buffer returned")
	}
	if n := len(buf.items); n != 0 {
		t.Errorf("new scope should be empty, got %d items", n)
	}
}

// ensureScope must charge scopeBufferOverhead against totalBytes so a
// later /admin /delete_scope releases exactly what was reserved.
// Without this, deleteScope's unconditional `-(scopeBytes + overhead)`
// would underflow totalBytes by 1024 bytes per cycle on these
// internal counter scopes — bounded, but a real invariant break.
func TestStore_EnsureScope_ReservesOverheadAndRoundTrips(t *testing.T) {
	s := newStore(Config{ScopeMaxItems: 10, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})

	before := s.totalBytes.Load()
	buf := s.ensureScope("_counters_count_calls")
	if buf == nil {
		t.Fatal("ensureScope returned nil with ample cap")
	}
	if got := s.totalBytes.Load() - before; got != int64(scopeBufferOverhead) {
		t.Fatalf("ensureScope reserved %d bytes; want %d (scopeBufferOverhead)", got, scopeBufferOverhead)
	}

	if _, ok, _ := s.deleteScope("_counters_count_calls"); !ok {
		t.Fatal("deleteScope reported miss on the freshly ensured scope")
	}
	if got := s.totalBytes.Load(); got != before {
		t.Errorf("totalBytes drift after ensureScope+deleteScope round-trip: got=%d want=%d", got, before)
	}
}

func TestStore_EnsureScope_Idempotent(t *testing.T) {
	s := newStore(Config{ScopeMaxItems: 10, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})
	b1 := s.ensureScope("_counters_count_calls")
	b2 := s.ensureScope("_counters_count_calls")
	if b1 != b2 {
		t.Fatal("repeat ensureScope should return same buffer")
	}
}

// ensureScope under concurrent access must not double-create or panic.
func TestStore_EnsureScope_Concurrent(t *testing.T) {
	s := newStore(Config{ScopeMaxItems: 10, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})

	const N = 50
	bufs := make([]*scopeBuffer, N)
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func(idx int) {
			defer wg.Done()
			bufs[idx] = s.ensureScope("_counters_count_calls")
		}(i)
	}
	wg.Wait()

	first := bufs[0]
	for i, b := range bufs {
		if b != first {
			t.Errorf("ensureScope returned different buffer at idx %d", i)
		}
	}
}

func TestStore_DeleteScope(t *testing.T) {
	s := newStore(Config{ScopeMaxItems: 10, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})
	buf, _ := s.getOrCreateScope("x")
	_, _ = buf.appendItem(newItem("x", "a", nil))
	_, _ = buf.appendItem(newItem("x", "b", nil))

	n, ok, err := s.deleteScope("x")
	if err != nil || !ok || n != 2 {
		t.Fatalf("deleteScope: ok=%v n=%d err=%v", ok, n, err)
	}
	if _, found := s.getScope("x"); found {
		t.Fatal("scope should be gone")
	}

	n, ok, err = s.deleteScope("missing")
	if err != nil || ok || n != 0 {
		t.Fatalf("deleteScope(missing): ok=%v n=%d err=%v", ok, n, err)
	}

	// Empty scope is a shape bug from the caller — the validator refuses
	// it up front, returning (0, false, ErrInvalidInput).
	n, ok, err = s.deleteScope("")
	if !errors.Is(err, ErrInvalidInput) || ok || n != 0 {
		t.Fatalf("deleteScope(\"\"): ok=%v n=%d err=%v want ErrInvalidInput", ok, n, err)
	}
}

// --- store-level byte budget --------------------------------------------------

// reserveBytes must not fail OPEN when MaxStoreBytes is configured near
// math.MaxInt64. The naive form `current + delta > maxStoreBytes` wraps
// to negative on overflow, the comparison passes, and the cap-violating
// reservation is admitted. The overflow-safe form
// `delta > maxStoreBytes - current` rejects correctly.
//
// Repro: cap = MaxInt64, pre-load current near MaxInt64, then try to
// reserve a delta whose sum overflows. Pre-fix this returns ok=true;
// post-fix it must return ok=false.
func TestStore_ReserveBytes_DoesNotFailOpenOnInt64Overflow(t *testing.T) {
	s := newStore(Config{ScopeMaxItems: 100, MaxStoreBytes: math.MaxInt64, MaxItemBytes: 1 << 20})
	s.totalBytes.Store(math.MaxInt64 - 100)

	ok, current, gotCap := s.reserveBytes(200)
	if ok {
		t.Fatalf("reserve 200 with current=MaxInt64-100 must fail (would overflow), got ok=true total=%d", current)
	}
	if current != math.MaxInt64-100 {
		t.Fatalf("current=%d want %d (unchanged on failed reserve)", current, math.MaxInt64-100)
	}
	if gotCap != math.MaxInt64 {
		t.Fatalf("cap=%d want MaxInt64", gotCap)
	}
	// Sanity: a delta that fits must still succeed at the boundary.
	if ok, _, _ := s.reserveBytes(50); !ok {
		t.Fatal("reserve 50 (fits in remaining 100) must succeed")
	}
}

// addClampedInt64 saturates instead of wrapping at the int64
// boundary. The request-body cap helpers (bulkRequestBytesFor,
// singleRequestBytesFor) use it so a near-MaxInt64 operator cap can't
// produce a negative request-body cap that silently rejects every
// write.
func TestAddClampedInt64(t *testing.T) {
	cases := []struct {
		a, b, want int64
	}{
		{0, 0, 0},
		{1, 2, 3},
		{-5, 3, -2},
		{math.MaxInt64 - 10, 5, math.MaxInt64 - 5},
		{math.MaxInt64 - 10, 100, math.MaxInt64}, // overflow → clamp
		{math.MaxInt64, 1, math.MaxInt64},        // overflow → clamp
		{math.MinInt64 + 10, -5, math.MinInt64 + 5},
		{math.MinInt64 + 10, -100, math.MinInt64}, // underflow → clamp
		{math.MinInt64, -1, math.MinInt64},        // underflow → clamp
	}
	for _, tc := range cases {
		if got := addClampedInt64(tc.a, tc.b); got != tc.want {
			t.Errorf("addClampedInt64(%d, %d) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

// deleteScope must not race with an /append that obtained the buf pointer
// before the scope was removed from the map. Under the old RLock-snapshot
// pattern, the appended item's bytes leaked into s.totalBytes after the
// subtract happened on a stale value. This test drives many rounds of
// parallel append/delete on the same scope and asserts the final counter
// matches the items that survived in s.scopes.
func TestStore_DeleteScope_RaceWithAppend(t *testing.T) {
	s := newStore(Config{ScopeMaxItems: 1000, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})

	const rounds = 200
	for i := 0; i < rounds; i++ {
		scope := "race"
		buf, _ := s.getOrCreateScope(scope)
		// Prime with one item so deleteScope has real bytes to subtract.
		if _, err := buf.appendItem(newItem(scope, "", nil)); err != nil {
			t.Fatalf("prime append: %v", err)
		}

		done := make(chan struct{}, 2)
		go func() {
			buf, _ := s.getOrCreateScope(scope)
			_, _ = buf.appendItem(newItem(scope, "", nil))
			done <- struct{}{}
		}()
		go func() {
			_, _, _ = s.deleteScope(scope)
			done <- struct{}{}
		}()
		<-done
		<-done
	}

	// Final invariant: s.totalBytes == sum(buf.bytes) + scope-overhead
	// per live scope. Any ghost bytes from the race would inflate
	// totalBytes above the sum.
	var liveBytes int64
	live := s.listScopes()
	for _, buf := range live {
		buf.mu.RLock()
		liveBytes += buf.bytes
		buf.mu.RUnlock()
	}
	expected := liveBytes + int64(len(live))*scopeBufferOverhead
	if got := s.totalBytes.Load(); got != expected {
		t.Fatalf("totalBytes=%d but live scopes hold %d bytes + %d overhead = %d (ghost bytes from race)",
			got, liveBytes, int64(len(live))*scopeBufferOverhead, expected)
	}
}

// --- /stats counter invariants -----------------------------------------------

// assertStatsCountersInvariant walks every shard and verifies the
// invariants that make the O(1) /stats shape correct:
//
//   - s.totalItems  == Σ len(buf.items) over every live scope buffer
//   - s.scopeCount  == Σ len(sh.scopes) over every shard
//   - s.lastWriteTS >= max(buf.lastWriteTS) over every live scope buffer
//
// Forgetting to update one of these counters from a write/delete/bulk
// path silently corrupts /stats output without affecting any cache
// behaviour — exactly the class of bug a routine assertion catches and
// a hand-test never finds. Call this after any sequence of mutations.
//
// The lastWriteTS check is `>=` not `==` because store-level
// destructive paths (deleteScope, wipe) bump s.lastWriteTS without
// leaving any per-scope b.lastWriteTS behind (the scope is gone), so
// after such an event the store-wide value is strictly greater than
// any surviving scope's.
//
// Takes per-scope read locks during the walk; safe to call from tests
// that have other goroutines mutating the store, but any concurrent
// mutation observed mid-walk is the caller's tolerance to weigh.
func assertStatsCountersInvariant(t *testing.T, s *store, ctx string) {
	t.Helper()

	var sumItems int64
	var sumScopes int64
	var maxScopeLastWriteTS int64
	for i := range s.shards {
		sh := &s.shards[i]
		sh.mu.RLock()
		sumScopes += int64(len(sh.scopes))
		for _, buf := range sh.scopes {
			buf.mu.RLock()
			sumItems += int64(len(buf.items))
			if buf.lastWriteTS > maxScopeLastWriteTS {
				maxScopeLastWriteTS = buf.lastWriteTS
			}
			buf.mu.RUnlock()
		}
		sh.mu.RUnlock()
	}

	if got := s.totalItems.Load(); got != sumItems {
		t.Errorf("[%s] totalItems=%d but Σ len(buf.items)=%d (counter drift)", ctx, got, sumItems)
	}
	if got := s.scopeCount.Load(); got != sumScopes {
		t.Errorf("[%s] scopeCount=%d but Σ len(sh.scopes)=%d (counter drift)", ctx, got, sumScopes)
	}
	if got := s.lastWriteTS.Load(); got < maxScopeLastWriteTS {
		t.Errorf("[%s] lastWriteTS=%d but max(buf.lastWriteTS)=%d (store-wide tick lags scope)", ctx, got, maxScopeLastWriteTS)
	}
}

// TestStore_LastWriteTS_StartsAtZero pins the "freshness sentinel"
// contract: a fresh store with no writes must report 0, so a polling
// client can use 0 as the unambiguous "I've never seen this cache
// before" marker.
func TestStore_LastWriteTS_StartsAtZero(t *testing.T) {
	s := newStore(Config{ScopeMaxItems: 10, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})
	if got := s.lastWriteTS.Load(); got != 0 {
		t.Errorf("fresh store lastWriteTS=%d want 0", got)
	}
}

// TestNewStore_NoPreCreatedScopes verifies that NewStore does not
// pre-create any scopes. All scopes are user-managed; they appear on
// first write and disappear on /delete_scope or /wipe.
func TestNewStore_NoPreCreatedScopes(t *testing.T) {
	s := newStore(Config{ScopeMaxItems: 10, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})

	for _, name := range []string{
		"thread:42",
		"events",
		"_tokens",     // addon-convention prefix; nothing special about underscore
		"_counters_x", // same
		"_",           // underscore alone
	} {
		if _, ok := s.getScope(name); ok {
			t.Errorf("scope %q exists on fresh store; no scope should be pre-created", name)
		}
	}
}

// TestStore_LastWriteTS_MonotonicUnderRace verifies the CAS-max
// guarantee: even under aggressive concurrent writers from many
// scopes (whose individual time.Now().UnixMicro() readings can
// land in the counter out of order), the store-wide counter only
// ever advances. The post-condition is the simplest possible:
// the final counter value equals the maximum of every per-write
// timestamp the workers observed.
func TestStore_LastWriteTS_MonotonicUnderRace(t *testing.T) {
	const (
		workers      = 16
		opsPerWorker = 200
	)
	s := newStore(Config{ScopeMaxItems: 1000, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})

	maxObserved := make([]int64, workers)
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func(id int) {
			defer wg.Done()
			scope := fmt.Sprintf("race_%d", id)
			for i := 0; i < opsPerWorker; i++ {
				if _, err := s.appendOne(Item{
					Scope:   scope,
					Payload: json.RawMessage(`"v"`),
				}); err != nil {
					t.Errorf("worker %d op %d: %v", id, i, err)
					return
				}
				if got := s.lastWriteTS.Load(); got > maxObserved[id] {
					maxObserved[id] = got
				}
			}
		}(w)
	}
	wg.Wait()

	final := s.lastWriteTS.Load()
	for id, v := range maxObserved {
		if final < v {
			t.Errorf("final lastWriteTS=%d < worker[%d] max-observed=%d (CAS-max regressed)", final, id, v)
		}
	}
	// Also assert >= max(buf.lastWriteTS) per the standard invariant.
	assertStatsCountersInvariant(t, s, "after race workload")
}
