package scopecache

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
)

func newItem(scope, id string, payload map[string]interface{}) Item {
	if payload == nil {
		payload = map[string]interface{}{"v": 1}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return Item{Scope: scope, ID: id, Payload: raw}
}

// newTestBuffer returns a scopeBuffer wired to a permissive private
// store: byte / item caps come from the store, so per-scope cap
// behaviour stays observable in tests that previously relied on the
// orphan-buffer ctor. Byte and per-item caps are wide open (1<<62)
// so only the explicit item-count cap (maxItems) fires; tests that
// want to exercise byte cap behaviour build a store directly via
// newStore(Config{...}).
func newTestBuffer(maxItems int) *scopeBuffer {
	s := newStore(Config{
		ScopeMaxItems: maxItems,
		MaxStoreBytes: 1 << 62,
		MaxItemBytes:  1 << 62,
	})
	return s.newscopeBuffer()
}

// --- scopeBuffer.appendItem ---------------------------------------------------

func TestAppendItem_AssignsSeqMonotonically(t *testing.T) {
	buf := newTestBuffer(10)

	for i := 1; i <= 5; i++ {
		it, err := buf.appendItem(newItem("s", "", nil))
		if err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
		if it.Seq != uint64(i) {
			t.Fatalf("append %d: seq=%d want %d", i, it.Seq, i)
		}
	}
}

func TestAppendItem_RejectsDuplicateID(t *testing.T) {
	buf := newTestBuffer(10)

	if _, err := buf.appendItem(newItem("s", "a", nil)); err != nil {
		t.Fatalf("first append: %v", err)
	}
	if _, err := buf.appendItem(newItem("s", "a", nil)); err == nil {
		t.Fatal("expected duplicate id rejection")
	}
}

func TestAppendItem_AllowsMultipleEmptyIDs(t *testing.T) {
	buf := newTestBuffer(10)

	for i := 0; i < 3; i++ {
		if _, err := buf.appendItem(newItem("s", "", nil)); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	if len(buf.items) != 3 {
		t.Fatalf("len=%d want 3", len(buf.items))
	}
}

// Capacity is a hard cap: a write that would push the buffer past maxItems
// is rejected with ScopeFullError. No eviction happens — state, seq cursor
// and byID index stay exactly as they were before the failed append.
func TestAppendItem_RejectsAtCapacity(t *testing.T) {
	buf := newTestBuffer(3)

	for i := 0; i < 3; i++ {
		if _, err := buf.appendItem(newItem("s", "", nil)); err != nil {
			t.Fatalf("pre-fill %d: %v", i, err)
		}
	}

	_, err := buf.appendItem(newItem("s", "overflow", nil))
	if err == nil {
		t.Fatal("expected ScopeFullError when appending past cap")
	}
	var sfe *ScopeFullError
	if !errors.As(err, &sfe) {
		t.Fatalf("expected *ScopeFullError, got %T: %v", err, err)
	}
	if sfe.Count != 3 || sfe.Cap != 3 {
		t.Fatalf("ScopeFullError{Count:%d Cap:%d}, want {3,3}", sfe.Count, sfe.Cap)
	}

	if len(buf.items) != 3 {
		t.Fatalf("rejected write mutated buffer: len=%d want 3", len(buf.items))
	}
	if buf.lastSeq != 3 {
		t.Fatalf("rejected write advanced lastSeq: got %d want 3", buf.lastSeq)
	}
	if _, ok := buf.byID["overflow"]; ok {
		t.Fatal("rejected id 'overflow' leaked into byID index")
	}
}

// --- scopeBuffer.replaceAll ---------------------------------------------------

func TestReplaceAll_AssignsFreshSeqFromOne(t *testing.T) {
	buf := newTestBuffer(10)
	_, _ = buf.appendItem(newItem("s", "", nil))
	_, _ = buf.appendItem(newItem("s", "", nil))

	items := []Item{
		newItem("s", "a", nil),
		newItem("s", "b", nil),
	}
	_, err := buf.replaceAll(items)
	if err != nil {
		t.Fatal(err)
	}

	if buf.items[0].Seq != 1 || buf.items[1].Seq != 2 {
		t.Fatalf("seq not reset: %+v", buf.items)
	}
	if buf.lastSeq != 2 {
		t.Fatalf("lastSeq=%d want 2", buf.lastSeq)
	}
}

// replaceAll rejects the whole batch when it exceeds the per-scope cap —
// no silent truncation. Pre-existing state must stay untouched since the
// buffer is the mutation target and the caller expects all-or-nothing.
func TestReplaceAll_RejectsOverCap(t *testing.T) {
	buf := newTestBuffer(3)
	_, _ = buf.appendItem(newItem("s", "keep", nil))
	priorLen := len(buf.items)

	items := []Item{
		newItem("s", "a", nil),
		newItem("s", "b", nil),
		newItem("s", "c", nil),
		newItem("s", "d", nil),
	}
	_, err := buf.replaceAll(items)
	if err == nil {
		t.Fatal("expected ScopeFullError when replacement exceeds cap")
	}
	var sfe *ScopeFullError
	if !errors.As(err, &sfe) {
		t.Fatalf("expected *ScopeFullError, got %T: %v", err, err)
	}
	if sfe.Count != 4 || sfe.Cap != 3 {
		t.Fatalf("ScopeFullError{Count:%d Cap:%d}, want {4,3}", sfe.Count, sfe.Cap)
	}
	if len(buf.items) != priorLen {
		t.Fatalf("rejected replaceAll mutated buffer: len=%d want %d", len(buf.items), priorLen)
	}
}

func TestReplaceAll_RejectsDuplicateIDs(t *testing.T) {
	buf := newTestBuffer(10)

	items := []Item{
		newItem("s", "a", nil),
		newItem("s", "a", nil),
	}
	if _, err := buf.replaceAll(items); err == nil {
		t.Fatal("expected duplicate id error")
	}
}

func TestReplaceAll_EmptyItemsClearsScope(t *testing.T) {
	buf := newTestBuffer(10)
	_, _ = buf.appendItem(newItem("s", "", nil))

	if _, err := buf.replaceAll([]Item{}); err != nil {
		t.Fatal(err)
	}
	if len(buf.items) != 0 {
		t.Fatalf("expected empty buffer, got %d items", len(buf.items))
	}
}

// --- scopeBuffer.updateByID ---------------------------------------------------

func TestUpdateByID_HitPreservesSeq(t *testing.T) {
	buf := newTestBuffer(10)
	original, _ := buf.appendItem(newItem("s", "a", map[string]interface{}{"v": 1}))

	newPayload, _ := json.Marshal(map[string]interface{}{"v": 2})
	n, err := buf.updateByID("a", newPayload, nil)
	if err != nil {
		t.Fatalf("updateByID: %v", err)
	}
	if n != 1 {
		t.Fatalf("updated=%d want 1", n)
	}

	got, _ := buf.getByID("a")
	if got.Seq != original.Seq {
		t.Fatalf("seq changed: %d -> %d", original.Seq, got.Seq)
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal(got.Payload, &decoded); err != nil {
		t.Fatalf("payload decode: %v", err)
	}
	if v, ok := decoded["v"].(float64); !ok || v != 2 {
		t.Fatalf("payload not updated: %s", string(got.Payload))
	}
}

func TestUpdateByID_Miss(t *testing.T) {
	buf := newTestBuffer(10)
	raw, _ := json.Marshal(map[string]interface{}{"v": 1})
	n, err := buf.updateByID("missing", raw, nil)
	if err != nil {
		t.Fatalf("updateByID: %v", err)
	}
	if n != 0 {
		t.Fatalf("updated=%d want 0", n)
	}
}

// --- scopeBuffer.updateBySeq --------------------------------------------------

func TestUpdateBySeq_Hit(t *testing.T) {
	buf := newTestBuffer(10)
	it, _ := buf.appendItem(newItem("s", "", map[string]interface{}{"v": 1}))

	newPayload, _ := json.Marshal(map[string]interface{}{"v": 2})
	n, err := buf.updateBySeq(it.Seq, newPayload, nil)
	if err != nil {
		t.Fatalf("updateBySeq: %v", err)
	}
	if n != 1 {
		t.Fatalf("updated=%d want 1", n)
	}

	got, _ := buf.getBySeq(it.Seq)
	var decoded map[string]interface{}
	if err := json.Unmarshal(got.Payload, &decoded); err != nil {
		t.Fatalf("payload decode: %v", err)
	}
	if v, ok := decoded["v"].(float64); !ok || v != 2 {
		t.Fatalf("payload not updated: %s", string(got.Payload))
	}
}

func TestUpdateBySeq_KeepsByIDIndexInSync(t *testing.T) {
	buf := newTestBuffer(10)
	it, _ := buf.appendItem(newItem("s", "a", map[string]interface{}{"v": 1}))

	newPayload, _ := json.Marshal(map[string]interface{}{"v": 42})
	if _, err := buf.updateBySeq(it.Seq, newPayload, nil); err != nil {
		t.Fatalf("updateBySeq: %v", err)
	}

	// byID index must reflect the new payload too, otherwise a /get by id
	// would return the pre-update payload.
	got, ok := buf.getByID("a")
	if !ok {
		t.Fatal("getByID missed after updateBySeq")
	}
	var decoded map[string]interface{}
	_ = json.Unmarshal(got.Payload, &decoded)
	if decoded["v"].(float64) != 42 {
		t.Fatalf("byID stale payload: %s", string(got.Payload))
	}
}

func TestUpdateBySeq_Miss(t *testing.T) {
	buf := newTestBuffer(10)
	raw, _ := json.Marshal(map[string]interface{}{"v": 1})
	n, err := buf.updateBySeq(999, raw, nil)
	if err != nil {
		t.Fatalf("updateBySeq: %v", err)
	}
	if n != 0 {
		t.Fatalf("updated=%d want 0", n)
	}
}

// --- scopeBuffer.upsertByID ---------------------------------------------------

func TestUpsertByID_CreatesNewItem(t *testing.T) {
	buf := newTestBuffer(10)

	result, created, err := buf.upsertByID(newItem("s", "a", map[string]interface{}{"v": 1}))
	if err != nil {
		t.Fatalf("upsertByID: %v", err)
	}
	if !created {
		t.Fatal("created=false on first upsert")
	}
	if result.Seq != 1 {
		t.Fatalf("seq=%d want 1", result.Seq)
	}
	if _, ok := buf.byID["a"]; !ok {
		t.Fatal("byID index missing new item")
	}
	if len(buf.items) != 1 {
		t.Fatalf("items len=%d want 1", len(buf.items))
	}
}

func TestUpsertByID_ReplacesPayloadAndPreservesSeq(t *testing.T) {
	buf := newTestBuffer(10)
	first, _, err := buf.upsertByID(newItem("s", "a", map[string]interface{}{"v": 1}))
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}

	second, created, err := buf.upsertByID(newItem("s", "a", map[string]interface{}{"v": 2}))
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if created {
		t.Fatal("created=true on replace")
	}
	if second.Seq != first.Seq {
		t.Fatalf("seq changed: %d -> %d", first.Seq, second.Seq)
	}
	if len(buf.items) != 1 {
		t.Fatalf("items len=%d want 1 (no duplicate inserted)", len(buf.items))
	}

	got, _ := buf.getByID("a")
	var decoded map[string]interface{}
	_ = json.Unmarshal(got.Payload, &decoded)
	if decoded["v"].(float64) != 2 {
		t.Fatalf("payload not replaced: %s", string(got.Payload))
	}
}

func TestUpsertByID_CoexistsWithAppend(t *testing.T) {
	buf := newTestBuffer(10)
	_, _ = buf.appendItem(newItem("s", "a", nil))

	result, created, err := buf.upsertByID(newItem("s", "b", map[string]interface{}{"v": 9}))
	if err != nil {
		t.Fatalf("upsertByID: %v", err)
	}
	if !created {
		t.Fatal("created=false for a fresh id")
	}
	if result.Seq != 2 {
		t.Fatalf("seq=%d want 2 (continuous with prior append)", result.Seq)
	}
}

func TestUpsertByID_RejectsAtCapacity(t *testing.T) {
	buf := newTestBuffer(2)
	_, _ = buf.appendItem(newItem("s", "a", nil))
	_, _ = buf.appendItem(newItem("s", "b", nil))

	_, _, err := buf.upsertByID(newItem("s", "c", nil))
	if err == nil {
		t.Fatal("expected ScopeFullError when upserting past cap")
	}
	var sfe *ScopeFullError
	if !errors.As(err, &sfe) {
		t.Fatalf("expected *ScopeFullError, got %T: %v", err, err)
	}

	// A replace must still succeed at capacity — only create hits the cap.
	if _, _, err := buf.upsertByID(newItem("s", "a", map[string]interface{}{"v": 99})); err != nil {
		t.Fatalf("replace at cap should succeed: %v", err)
	}
}

// --- scopeBuffer.deleteByID ---------------------------------------------------

func TestDeleteByID_Hit(t *testing.T) {
	buf := newTestBuffer(10)
	_, _ = buf.appendItem(newItem("s", "a", nil))
	_, _ = buf.appendItem(newItem("s", "b", nil))

	n, _ := buf.deleteByID("a")
	if n != 1 {
		t.Fatalf("deleted=%d want 1", n)
	}
	if _, ok := buf.byID["a"]; ok {
		t.Fatal("id 'a' still in index")
	}
	if len(buf.items) != 1 {
		t.Fatalf("len=%d want 1", len(buf.items))
	}
}

func TestDeleteByID_Miss(t *testing.T) {
	buf := newTestBuffer(10)
	n, _ := buf.deleteByID("missing")
	if n != 0 {
		t.Fatalf("deleted=%d want 0", n)
	}
}

func TestDeleteByID_DoesNotRollbackLastSeq(t *testing.T) {
	buf := newTestBuffer(10)
	_, _ = buf.appendItem(newItem("s", "a", nil))
	_, _ = buf.appendItem(newItem("s", "b", nil))

	_, _ = buf.deleteByID("b")
	next, _ := buf.appendItem(newItem("s", "c", nil))
	if next.Seq != 3 {
		t.Fatalf("seq=%d want 3 (no rollback)", next.Seq)
	}
}

// Drain-to-empty via single-item deletes must release the high-
// watermark backing storage. Pre-fix the items slice's reslice kept
// cap pinned at the historical max, and Go maps never shrink their
// bucket arrays on delete() — a write-buffer scope that drained 1k
// items down to 0 leaked ~100 KiB of capacity until appendItem
// re-grew it. resetIfEmptyLocked nil's items + maps when len drops
// to zero so the GC can reclaim everything.
//
// Asserts the post-conditions resetIfEmptyLocked promises:
//   - items slice is nil (cap == 0; no backing array retained)
//   - bySeq / byID maps are nil
//   - lastSeq is preserved (monotonic across drain/refill cycles)
//   - subsequent appendItem still works (lazy-init verified)
func TestDeleteByID_DrainToEmptyReleasesBacking(t *testing.T) {
	buf := newTestBuffer(10_000)
	const N = 1000
	for i := 0; i < N; i++ {
		_, _ = buf.appendItem(newItem("s", fmt.Sprintf("id-%d", i), nil))
	}
	if cap(buf.items) < N {
		t.Fatalf("setup: cap(items)=%d expected at least %d", cap(buf.items), N)
	}
	preDrainLastSeq := buf.lastSeq

	for i := 0; i < N; i++ {
		if n, _ := buf.deleteByID(fmt.Sprintf("id-%d", i)); n != 1 {
			t.Fatalf("delete id-%d: n=%d want 1", i, n)
		}
	}

	if cap(buf.items) != 0 {
		t.Errorf("cap(items)=%d after drain-to-empty, want 0 (backing array not released)", cap(buf.items))
	}
	if buf.items != nil {
		t.Errorf("items slice = %v, want nil", buf.items)
	}
	if buf.bySeq != nil {
		t.Errorf("bySeq = %v, want nil (map buckets not released)", buf.bySeq)
	}
	if buf.byID != nil {
		t.Errorf("byID = %v, want nil (map buckets not released)", buf.byID)
	}
	if buf.lastSeq != preDrainLastSeq {
		t.Errorf("lastSeq=%d, want %d (cursor must not regress on drain)", buf.lastSeq, preDrainLastSeq)
	}

	// appendItem must lazy-init the slice and maps after a reset.
	next, err := buf.appendItem(newItem("s", "after-drain", nil))
	if err != nil {
		t.Fatalf("appendItem after drain: %v", err)
	}
	if next.Seq <= preDrainLastSeq {
		t.Errorf("post-drain append seq=%d, want > %d (cursor regressed)", next.Seq, preDrainLastSeq)
	}
}

// deleteUpToSeq draining everything must also nil the maps. The items
// slice is already replaced by `rest := make(...)` in the existing
// code; the map-side leak is what resetIfEmptyLocked closes here.
func TestDeleteUpToSeq_DrainToEmptyReleasesBacking(t *testing.T) {
	buf := newTestBuffer(10_000)
	const N = 1000
	var lastSeq uint64
	for i := 0; i < N; i++ {
		it, _ := buf.appendItem(newItem("s", fmt.Sprintf("id-%d", i), nil))
		lastSeq = it.Seq
	}

	if n, _ := buf.deleteUpToSeq(lastSeq); n != N {
		t.Fatalf("deleteUpToSeq: n=%d want %d", n, N)
	}

	if cap(buf.items) != 0 {
		t.Errorf("cap(items)=%d after drain, want 0", cap(buf.items))
	}
	if buf.bySeq != nil {
		t.Errorf("bySeq = %v, want nil", buf.bySeq)
	}
	if buf.byID != nil {
		t.Errorf("byID = %v, want nil", buf.byID)
	}
}

// --- scopeBuffer.deleteBySeq --------------------------------------------------

func TestDeleteBySeq_Hit(t *testing.T) {
	buf := newTestBuffer(10)
	_, _ = buf.appendItem(newItem("s", "a", nil))
	it2, _ := buf.appendItem(newItem("s", "b", nil))
	_, _ = buf.appendItem(newItem("s", "c", nil))

	n, _ := buf.deleteBySeq(it2.Seq)
	if n != 1 {
		t.Fatalf("deleted=%d want 1", n)
	}
	if _, ok := buf.bySeq[it2.Seq]; ok {
		t.Fatal("seq still in bySeq index")
	}
	if _, ok := buf.byID["b"]; ok {
		t.Fatal("id 'b' still in byID index")
	}
	if len(buf.items) != 2 {
		t.Fatalf("len=%d want 2", len(buf.items))
	}
}

func TestDeleteBySeq_Miss(t *testing.T) {
	buf := newTestBuffer(10)
	_, _ = buf.appendItem(newItem("s", "a", nil))

	if n, _ := buf.deleteBySeq(999); n != 0 {
		t.Fatalf("deleted=%d want 0", n)
	}
	if len(buf.items) != 1 {
		t.Fatalf("len=%d want 1", len(buf.items))
	}
}

func TestDeleteBySeq_NoIDItem(t *testing.T) {
	buf := newTestBuffer(10)
	it, _ := buf.appendItem(newItem("s", "", nil))

	if n, _ := buf.deleteBySeq(it.Seq); n != 1 {
		t.Fatalf("deleted=%d want 1", n)
	}
	if len(buf.items) != 0 {
		t.Fatalf("len=%d want 0", len(buf.items))
	}
}

func TestDeleteBySeq_DoesNotRollbackLastSeq(t *testing.T) {
	buf := newTestBuffer(10)
	_, _ = buf.appendItem(newItem("s", "a", nil))
	it2, _ := buf.appendItem(newItem("s", "b", nil))

	_, _ = buf.deleteBySeq(it2.Seq)
	next, _ := buf.appendItem(newItem("s", "c", nil))
	if next.Seq != 3 {
		t.Fatalf("seq=%d want 3 (no rollback)", next.Seq)
	}
}

// --- scopeBuffer.deleteUpToSeq ---------------------------------------------

func TestDeleteUpToSeq_RemovesPrefix(t *testing.T) {
	buf := newTestBuffer(10)
	for i := 1; i <= 5; i++ {
		_, _ = buf.appendItem(newItem("s", "", nil))
	}

	n, _ := buf.deleteUpToSeq(3)
	if n != 3 {
		t.Fatalf("deleted=%d want 3", n)
	}
	if len(buf.items) != 2 {
		t.Fatalf("len=%d want 2", len(buf.items))
	}
	if buf.items[0].Seq != 4 || buf.items[1].Seq != 5 {
		t.Fatalf("unexpected survivors: %+v", buf.items)
	}
	for seq := uint64(1); seq <= 3; seq++ {
		if _, ok := buf.bySeq[seq]; ok {
			t.Fatalf("seq %d should be gone from bySeq", seq)
		}
	}
}

func TestDeleteUpToSeq_RemovesIDsToo(t *testing.T) {
	buf := newTestBuffer(10)
	_, _ = buf.appendItem(newItem("s", "a", nil))
	_, _ = buf.appendItem(newItem("s", "b", nil))
	_, _ = buf.appendItem(newItem("s", "c", nil))

	_, _ = buf.deleteUpToSeq(2)

	if _, ok := buf.byID["a"]; ok {
		t.Fatal("id 'a' should have been removed from byID")
	}
	if _, ok := buf.byID["b"]; ok {
		t.Fatal("id 'b' should have been removed from byID")
	}
	if _, ok := buf.byID["c"]; !ok {
		t.Fatal("id 'c' should still exist")
	}
}

func TestDeleteUpToSeq_NoOpBelowRange(t *testing.T) {
	buf := newTestBuffer(5)
	// Append 3 items, then delete through the prefix before seq 3's start.
	// Nothing matches because no item has seq <= 0.
	for i := 1; i <= 3; i++ {
		_, _ = buf.appendItem(newItem("s", "", nil))
	}
	// Drop seqs 1..2 first to simulate a post-drain state, then ask to drop
	// anything <= 2 again. The cut point is already past — expect no-op.
	_, _ = buf.deleteUpToSeq(2)

	n, _ := buf.deleteUpToSeq(2)
	if n != 0 {
		t.Fatalf("deleted=%d want 0 (no items at or below seq 2 remain)", n)
	}
	if len(buf.items) != 1 {
		t.Fatalf("len=%d want 1", len(buf.items))
	}
}

func TestDeleteUpToSeq_RemovesAllWhenMaxAtOrAboveLast(t *testing.T) {
	buf := newTestBuffer(10)
	for i := 1; i <= 3; i++ {
		_, _ = buf.appendItem(newItem("s", "", nil))
	}

	n, _ := buf.deleteUpToSeq(99)
	if n != 3 {
		t.Fatalf("deleted=%d want 3", n)
	}
	if len(buf.items) != 0 {
		t.Fatalf("expected empty scope, got %d", len(buf.items))
	}
}

func TestDeleteUpToSeq_DoesNotRollbackLastSeq(t *testing.T) {
	buf := newTestBuffer(10)
	for i := 1; i <= 3; i++ {
		_, _ = buf.appendItem(newItem("s", "", nil))
	}

	_, _ = buf.deleteUpToSeq(3)
	next, _ := buf.appendItem(newItem("s", "", nil))
	if next.Seq != 4 {
		t.Fatalf("seq=%d want 4 (no rollback after draining)", next.Seq)
	}
}

func TestDeleteUpToSeq_ReleasesBackingArray(t *testing.T) {
	// Fill a scope well past its natural grow-cycle so the backing array
	// has capacity noticeably larger than the survivors. Drain the prefix
	// and assert the backing array was reallocated to match the remainder
	// — that is the guarantee that frees the removed-payload memory for
	// GC in the write-buffer drain-from-front pattern.
	const fill = 1000
	buf := newTestBuffer(fill * 2)
	for i := 0; i < fill; i++ {
		_, _ = buf.appendItem(newItem("s", "", nil))
	}
	preCap := cap(buf.items)
	if preCap < fill {
		t.Fatalf("sanity: pre-drain cap=%d want >= %d", preCap, fill)
	}

	drained, _ := buf.deleteUpToSeq(uint64(fill - 10))
	if drained != fill-10 {
		t.Fatalf("drained=%d want %d", drained, fill-10)
	}
	if len(buf.items) != 10 {
		t.Fatalf("len=%d want 10", len(buf.items))
	}
	if cap(buf.items) != len(buf.items) {
		t.Fatalf("backing array not released: cap=%d len=%d (pre-drain cap was %d)",
			cap(buf.items), len(buf.items), preCap)
	}
}

// --- scopeBuffer.tailOffset ---------------------------------------------------

func TestTailOffset_BasicAndEdges(t *testing.T) {
	buf := newTestBuffer(10)
	for i := 1; i <= 5; i++ {
		_, _ = buf.appendItem(newItem("s", "", nil))
	}

	tests := []struct {
		limit, offset int
		wantSeq       []uint64
	}{
		{2, 0, []uint64{4, 5}},
		{2, 2, []uint64{2, 3}},
		{10, 0, []uint64{1, 2, 3, 4, 5}},
		{2, 10, nil},
	}

	for _, tc := range tests {
		got, _ := buf.tailOffset(tc.limit, tc.offset)
		if len(got) != len(tc.wantSeq) {
			t.Errorf("tail(limit=%d offset=%d): len=%d want %d", tc.limit, tc.offset, len(got), len(tc.wantSeq))
			continue
		}
		for i, seq := range tc.wantSeq {
			if got[i].Seq != seq {
				t.Errorf("tail(limit=%d offset=%d)[%d].seq=%d want %d", tc.limit, tc.offset, i, got[i].Seq, seq)
			}
		}
	}
}

// --- scopeBuffer.sinceSeq -----------------------------------------------------

func TestSinceSeq_ReturnsItemsAfterCursor(t *testing.T) {
	buf := newTestBuffer(10)
	for i := 1; i <= 5; i++ {
		_, _ = buf.appendItem(newItem("s", "", nil))
	}

	// Limit chosen above the available count to confirm "all matching"
	// behaviour without relying on the historical limit=0 → unlimited
	// semantics (tightened to limit ≤ 0 → empty for cross-method
	// uniformity; see sinceSeq's doc-comment).
	got, _ := buf.sinceSeq(2, 100)
	if len(got) != 3 {
		t.Fatalf("len=%d want 3", len(got))
	}
	if got[0].Seq != 3 {
		t.Fatalf("first.seq=%d want 3", got[0].Seq)
	}
}

func TestSinceSeq_RespectsLimit(t *testing.T) {
	buf := newTestBuffer(10)
	for i := 1; i <= 5; i++ {
		_, _ = buf.appendItem(newItem("s", "", nil))
	}

	got, _ := buf.sinceSeq(0, 2)
	if len(got) != 2 {
		t.Fatalf("len=%d want 2", len(got))
	}
}

func TestSinceSeq_EmptyWhenPastEnd(t *testing.T) {
	buf := newTestBuffer(10)
	_, _ = buf.appendItem(newItem("s", "", nil))

	got, _ := buf.sinceSeq(100, 100)
	if len(got) != 0 {
		t.Fatalf("len=%d want 0", len(got))
	}
}

// limit ≤ 0 returns an empty slice on every multi-item read
// (sinceSeq, tailOffset, scopeList). Pre-fix sinceSeq treated 0 as
// "no truncation" and returned every matching item — inconsistent
// with the other two methods, surprising for Go-API callers that
// passed an uninitialised int, and silently bypassing the
// HTTP-layer's normalizeLimit gate. The guard makes "give me ≤ 0
// items" answered uniformly with the empty result.
func TestSinceSeq_NonPositiveLimitReturnsEmpty(t *testing.T) {
	buf := newTestBuffer(10)
	for i := 1; i <= 5; i++ {
		_, _ = buf.appendItem(newItem("s", "", nil))
	}

	for _, limit := range []int{0, -1, -1000} {
		got, more := buf.sinceSeq(0, limit)
		if len(got) != 0 {
			t.Errorf("limit=%d: len=%d want 0", limit, len(got))
		}
		if more {
			t.Errorf("limit=%d: more=true want false", limit)
		}
	}
}

// --- scopeBuffer.getByID / getBySeq -------------------------------------------

func TestGetByIDAndSeq(t *testing.T) {
	buf := newTestBuffer(10)
	it, _ := buf.appendItem(newItem("s", "a", nil))

	if got, ok := buf.getByID("a"); !ok || got.Seq != it.Seq {
		t.Fatalf("getByID: ok=%v seq=%d want %d", ok, got.Seq, it.Seq)
	}
	if got, ok := buf.getBySeq(it.Seq); !ok || got.ID != "a" {
		t.Fatalf("getBySeq: ok=%v id=%q", ok, got.ID)
	}
	if _, ok := buf.getByID("missing"); ok {
		t.Fatal("getByID('missing') should miss")
	}
	if _, ok := buf.getBySeq(999); ok {
		t.Fatal("getBySeq(999) should miss")
	}
}

// --- recordRead ---------------------------------------------------------------

// recordRead bumps two atomics: readCountTotal and lastAccessTS. The
// time-windowed aggregation that used to live here moved out of core
// — see buffer_heat.go.
func TestRecordRead_BumpsCountAndTimestamp(t *testing.T) {
	buf := newTestBuffer(10)

	if got := buf.readCountTotal.Load(); got != 0 {
		t.Fatalf("pre-read readCountTotal=%d want 0", got)
	}
	if got := buf.lastAccessTS.Load(); got != 0 {
		t.Fatalf("pre-read lastAccessTS=%d want 0", got)
	}

	now := nowUnixMicro()
	buf.recordRead(now)
	buf.recordRead(now + 1)
	buf.recordRead(now + 2)

	if got := buf.readCountTotal.Load(); got != 3 {
		t.Errorf("readCountTotal=%d want 3", got)
	}
	if got := buf.lastAccessTS.Load(); got != now+2 {
		t.Errorf("lastAccessTS=%d want %d (most-recent stamp wins)", got, now+2)
	}
}

// --- approxSizeBytes ----------------------------------------------------------

func TestApproxSizeBytes_IgnoresReservedCapacity(t *testing.T) {
	buf := newTestBuffer(10000)
	size := buf.approxSizeBytes()

	// Buggy code counted cap(items)*32 = 320KB for an empty scope.
	if size > 2048 {
		t.Fatalf("empty scope approx_scope_bytes=%d want < 2KB (should not count reserved capacity)", size)
	}
}

func TestApproxSizeBytes_GrowsWithItems(t *testing.T) {
	buf := newTestBuffer(10000)
	before := buf.approxSizeBytes()

	_, _ = buf.appendItem(newItem("s", "a", map[string]interface{}{"text": "hello world"}))

	after := buf.approxSizeBytes()
	if after <= before {
		t.Fatalf("size did not grow after append: before=%d after=%d", before, after)
	}
}

// TestDeleteByID_ClearsBackingSlot verifies the GC invariant for deleteByID:
// after the slice shift-and-shrink, the tail slot must be nil-ed so the
// removed *Item is eligible for GC. The backing array still exists at full
// capacity, so we reslice past the current length to peek at the vacated slot.
func TestDeleteByID_ClearsBackingSlot(t *testing.T) {
	buf := newTestBuffer(8)

	_, _ = buf.appendItem(newItem("s", "a", map[string]interface{}{"marker": "A"}))
	_, _ = buf.appendItem(newItem("s", "b", map[string]interface{}{"marker": "B"}))
	_, _ = buf.appendItem(newItem("s", "c", map[string]interface{}{"marker": "C"}))

	if n, _ := buf.deleteByID("b"); n != 1 {
		t.Fatalf("delete: n=%d want 1", n)
	}
	if len(buf.items) != 2 {
		t.Fatalf("len=%d want 2 after delete", len(buf.items))
	}

	full := buf.items[:3]
	if tail := full[2]; tail != nil {
		t.Fatalf("tail slot not nil-ed in backing array: %+v", *tail)
	}
}

// --- lastWriteTS --------------------------------------------------------------
//
// lastWriteTS advances on every path that mutates the scope (append,
// upsert, update, counter_add, delete, deleteUpToSeq, replaceAll) and
// stays put on reads (recordRead). The "preCall <= lastWriteTS" bracket
// is the resilient assertion shape: clock resolution on Windows can be
// ~16ms, so a strictly-greater check against createdTS would be flaky;
// tests instead assert that lastWriteTS sits at or beyond a stamp
// captured immediately before the write call.

func TestLastWriteTS_FreshScopeEqualsCreatedTS(t *testing.T) {
	buf := newTestBuffer(10)
	if buf.lastWriteTS != buf.createdTS {
		t.Fatalf("fresh scope: lastWriteTS=%d createdTS=%d (must be equal — both initialised from one nowUnixMicro() call)",
			buf.lastWriteTS, buf.createdTS)
	}
	if buf.lastWriteTS == 0 {
		t.Fatal("fresh scope: lastWriteTS=0 (must be initialised to a real timestamp, not left zero)")
	}
}

func TestLastWriteTS_AdvancesOnAppend(t *testing.T) {
	buf := newTestBuffer(10)
	pre := nowUnixMicro()
	it, err := buf.appendItem(newItem("s", "", nil))
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if buf.lastWriteTS < pre {
		t.Errorf("lastWriteTS=%d pre=%d (must be >= pre-call stamp)", buf.lastWriteTS, pre)
	}
	if buf.lastWriteTS != it.Ts {
		t.Errorf("lastWriteTS=%d item.Ts=%d (insertNewItemLocked must use one nowUs for both)",
			buf.lastWriteTS, it.Ts)
	}
}

func TestLastWriteTS_AdvancesOnUpsertReplace(t *testing.T) {
	buf := newTestBuffer(10)
	if _, _, err := buf.upsertByID(newItem("s", "a", nil)); err != nil {
		t.Fatalf("upsert create: %v", err)
	}
	beforeReplace := buf.lastWriteTS

	pre := nowUnixMicro()
	if _, _, err := buf.upsertByID(newItem("s", "a", map[string]interface{}{"v": 2})); err != nil {
		t.Fatalf("upsert replace: %v", err)
	}
	if buf.lastWriteTS < pre {
		t.Errorf("lastWriteTS=%d pre=%d (replace path must stamp lastWriteTS)", buf.lastWriteTS, pre)
	}
	if buf.lastWriteTS < beforeReplace {
		t.Errorf("lastWriteTS=%d went backwards from %d (replace must not regress the timestamp)",
			buf.lastWriteTS, beforeReplace)
	}
}

func TestLastWriteTS_AdvancesOnUpdate(t *testing.T) {
	buf := newTestBuffer(10)
	if _, err := buf.appendItem(newItem("s", "a", nil)); err != nil {
		t.Fatalf("append: %v", err)
	}

	pre := nowUnixMicro()
	if _, err := buf.updateByID("a", json.RawMessage(`{"v":2}`), nil); err != nil {
		t.Fatalf("update: %v", err)
	}
	if buf.lastWriteTS < pre {
		t.Errorf("lastWriteTS=%d pre=%d (replaceItemAtIndexLocked must stamp lastWriteTS)",
			buf.lastWriteTS, pre)
	}
}

func TestLastWriteTS_AdvancesOnDelete(t *testing.T) {
	buf := newTestBuffer(10)
	for i := 0; i < 5; i++ {
		if _, err := buf.appendItem(newItem("s", "", map[string]interface{}{"i": i})); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	preDelByID := nowUnixMicro()
	if _, err := buf.deleteBySeq(2); err != nil {
		t.Fatalf("deleteBySeq: %v", err)
	}
	if buf.lastWriteTS < preDelByID {
		t.Errorf("after deleteBySeq: lastWriteTS=%d pre=%d", buf.lastWriteTS, preDelByID)
	}

	preDelUpTo := nowUnixMicro()
	if _, err := buf.deleteUpToSeq(4); err != nil {
		t.Fatalf("deleteUpToSeq: %v", err)
	}
	if buf.lastWriteTS < preDelUpTo {
		t.Errorf("after deleteUpToSeq: lastWriteTS=%d pre=%d", buf.lastWriteTS, preDelUpTo)
	}
}

func TestLastWriteTS_AdvancesOnReplaceAll(t *testing.T) {
	buf := newTestBuffer(10)
	if _, err := buf.appendItem(newItem("s", "a", nil)); err != nil {
		t.Fatalf("append: %v", err)
	}

	pre := nowUnixMicro()
	if _, err := buf.replaceAll([]Item{newItem("s", "x", nil), newItem("s", "y", nil)}); err != nil {
		t.Fatalf("replaceAll: %v", err)
	}
	if buf.lastWriteTS < pre {
		t.Errorf("after replaceAll (commitReplacement): lastWriteTS=%d pre=%d", buf.lastWriteTS, pre)
	}
}

// recordRead is a read-path bookkeeping update; it must not advance
// lastWriteTS. lastAccessTS is the matching read-side counter.
func TestLastWriteTS_NotAffectedByReads(t *testing.T) {
	buf := newTestBuffer(10)
	if _, err := buf.appendItem(newItem("s", "a", nil)); err != nil {
		t.Fatalf("append: %v", err)
	}
	beforeRead := buf.lastWriteTS

	buf.recordRead(nowUnixMicro())

	if buf.lastWriteTS != beforeRead {
		t.Errorf("recordRead changed lastWriteTS: before=%d after=%d (reads must not bump write timestamp)",
			beforeRead, buf.lastWriteTS)
	}
}

// stats() must surface lastWriteTS unchanged. Readers of the snapshot
// rely on this as the authoritative "when was this scope last written"
// signal — drift here would make /stats lie even though the underlying
// field is correct.
func TestStats_SurfacesLastWriteTS(t *testing.T) {
	buf := newTestBuffer(10)
	pre := nowUnixMicro()
	if _, err := buf.appendItem(newItem("s", "", nil)); err != nil {
		t.Fatalf("append: %v", err)
	}

	st := buf.stats()
	if st.LastWriteTS != buf.lastWriteTS {
		t.Errorf("stats.LastWriteTS=%d buf.lastWriteTS=%d (snapshot must mirror the field)",
			st.LastWriteTS, buf.lastWriteTS)
	}
	if st.LastWriteTS < pre {
		t.Errorf("stats.LastWriteTS=%d pre=%d", st.LastWriteTS, pre)
	}
}

// --- buildReplacementState ----------------------------------------------------

func TestBuildReplacementState_SeqFromOne(t *testing.T) {
	items := []Item{
		newItem("s", "a", nil),
		newItem("s", "b", nil),
		newItem("s", "c", nil),
	}
	r, err := buildReplacementState(items)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.items) != 3 {
		t.Fatalf("len=%d want 3 (no trim)", len(r.items))
	}
	if r.items[0].ID != "a" || r.items[2].ID != "c" {
		t.Fatalf("input order not preserved: %+v", r.items)
	}
	if r.items[0].Seq != 1 || r.items[2].Seq != 3 {
		t.Fatalf("seq not fresh: %+v", r.items)
	}
	if r.lastSeq != 3 {
		t.Fatalf("lastSeq=%d want 3", r.lastSeq)
	}
	if _, ok := r.byID["a"]; !ok {
		t.Fatal("byID missing 'a'")
	}
	if _, ok := r.bySeq[1]; !ok {
		t.Fatal("bySeq missing seq 1")
	}
}

// shrinkIfSparseLocked rebuilds the items slice + maps when the
// scope has gone substantially sparse — defined as cap > 1024 AND
// len < cap/4. Without it, draining 99% of a 100k-item scope leaves
// the 100k-element backing array + map buckets pinned until the
// next refill cycle. This test pushes a buffer past the threshold,
// drains heavily, and asserts the slice cap actually came down.
func TestBuffer_ShrinkIfSparseLocked_RebuildsSliceAndMaps(t *testing.T) {
	const N = 4096
	buf := newTestBuffer(N)

	for i := 1; i <= N; i++ {
		if _, err := buf.appendItem(newItem("s", fmt.Sprintf("id-%d", i), nil)); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	preCap := cap(buf.items)
	if preCap < shrinkMinCap*2 {
		t.Fatalf("test invalid: pre-shrink cap %d below threshold-times-two %d", preCap, shrinkMinCap*2)
	}

	// Drain enough that len drops below cap/4. Single-item deletes
	// to exercise the deleteIndexLocked → shrinkIfSparseLocked path
	// (deleteUpToSeq's own make-fresh-slice already shrinks the
	// items slice independently).
	keep := preCap/shrinkSparseRatio - 4
	for i := 1; i <= N-keep; i++ {
		if _, err := buf.deleteByID(fmt.Sprintf("id-%d", i)); err != nil {
			t.Fatalf("delete id-%d: %v", i, err)
		}
	}

	if got := cap(buf.items); got >= preCap {
		t.Errorf("slice cap did not shrink after drain: got %d, pre %d (sparse-rebuild missed)", got, preCap)
	}
	// After shrink the surviving items must still be reachable via
	// every address path — sanity-check both maps + slice consistency.
	if len(buf.items) != keep {
		t.Errorf("post-drain len(items)=%d want %d", len(buf.items), keep)
	}
	if len(buf.byID) != keep {
		t.Errorf("post-drain len(byID)=%d want %d (map should be re-sized to len)", len(buf.byID), keep)
	}
	if len(buf.bySeq) != keep {
		t.Errorf("post-drain len(bySeq)=%d want %d", len(buf.bySeq), keep)
	}
	// Spot-check that the kept items address-resolve.
	for _, item := range buf.items {
		if _, ok := buf.byID[item.ID]; !ok {
			t.Errorf("byID lost id=%s after rebuild", item.ID)
		}
		if _, ok := buf.bySeq[item.Seq]; !ok {
			t.Errorf("bySeq lost seq=%d after rebuild", item.Seq)
		}
	}
}

// Negative case: a buffer with cap below shrinkMinCap must NOT
// rebuild even when drained heavily. Avoids spurious allocation
// churn on small buffers where retention is bounded anyway.
func TestBuffer_ShrinkIfSparseLocked_SkipsBelowMinCap(t *testing.T) {
	buf := newTestBuffer(100)
	// Fewer items than shrinkMinCap so cap stays below the threshold.
	for i := 1; i <= 50; i++ {
		if _, err := buf.appendItem(newItem("s", fmt.Sprintf("id-%d", i), nil)); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	preCap := cap(buf.items)
	if preCap > shrinkMinCap {
		t.Skipf("buffer grew past shrinkMinCap (cap=%d) under N=50; test invariant changed", preCap)
	}

	// Drain to 1 item.
	for i := 1; i <= 49; i++ {
		if _, err := buf.deleteByID(fmt.Sprintf("id-%d", i)); err != nil {
			t.Fatalf("delete: %v", err)
		}
	}
	if cap(buf.items) != preCap {
		t.Errorf("small buffer rebuilt unnecessarily: pre=%d post=%d", preCap, cap(buf.items))
	}
}
