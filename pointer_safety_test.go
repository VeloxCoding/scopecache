// pointer_safety_test.go — guards for the value→pointer conversion of
// scopeBuffer's items/byID/bySeq indexes. Each test pins behaviour
// that must hold BOTH before the conversion (three independent value
// copies of every item) and after it (one shared *Item behind three
// indexes):
//
//   - buffer read methods return a value Item; mutating a returned
//     item's scalar fields never reaches stored state
//   - an in-place update or counter op is visible identically whether
//     the item is addressed by id, by seq, or scanned via tail
//   - a promote whose store-byte reservation fails leaves the original
//     item completely intact — no half-promoted state
//
// These run green on the value-type code; they are the regression
// gate the type-flip commit must keep green.

package scopecache

import (
	"encoding/json"
	"testing"
)

// TestPointerSafety_BufferReadReturnsValueCopy pins that getByID,
// getBySeq, tailOffset and sinceSeq hand back a value Item. Mutating
// a returned item's scalar fields must not change stored state — if a
// read method ever returned the live *Item this fails.
func TestPointerSafety_BufferReadReturnsValueCopy(t *testing.T) {
	buf := newscopeBuffer(100)
	for _, id := range []string{"a", "b", "c"} {
		if _, err := buf.appendItem(Item{Scope: "s", ID: id, Payload: json.RawMessage(`{"v":1}`)}); err != nil {
			t.Fatalf("append %s: %v", id, err)
		}
	}

	// "b" is the second append, so its seq is 2.
	assertUntouched := func(label string) {
		stored, ok := buf.getByID("b")
		if !ok {
			t.Fatalf("%s: getByID(b) miss", label)
		}
		if stored.Seq != 2 || stored.Scope != "s" {
			t.Errorf("%s: stored item mutated via a returned copy: %+v", label, stored)
		}
	}

	g1, _ := buf.getByID("b")
	g1.Seq, g1.Scope = 4242, "hijacked"
	assertUntouched("after getByID mutate")

	g2, ok := buf.getBySeq(2)
	if !ok {
		t.Fatal("getBySeq(2) miss")
	}
	g2.Seq, g2.Scope = 4242, "hijacked"
	assertUntouched("after getBySeq mutate")

	win, _ := buf.tailOffset(10, 0)
	for i := range win {
		win[i].Seq, win[i].Scope = 4242, "hijacked"
	}
	assertUntouched("after tailOffset mutate")

	since, _ := buf.sinceSeq(0, 10)
	for i := range since {
		since[i].Seq, since[i].Scope = 4242, "hijacked"
	}
	assertUntouched("after sinceSeq mutate")
}

// TestPointerSafety_UpdateCoherentAcrossAddressing pins that an
// in-place /update is visible identically whether the item is fetched
// by id, by seq, or scanned via tail. Broken index aliasing (one
// index pointing at a different struct than another) fails this.
func TestPointerSafety_UpdateCoherentAcrossAddressing(t *testing.T) {
	g := NewGateway(Config{ScopeMaxItems: 100, MaxStoreBytes: 10 << 20, MaxItemBytes: 1 << 20})

	appended, err := g.Append(Item{Scope: "s", ID: "x", Payload: json.RawMessage(`{"v":1}`)})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	seq := appended.Seq

	if n, err := g.Update(Item{Scope: "s", ID: "x", Payload: json.RawMessage(`{"v":2}`)}); err != nil || n != 1 {
		t.Fatalf("update: n=%d err=%v", n, err)
	}

	const want = `{"v":2}`
	if byID, ok := g.GetByID("s", "x"); !ok || string(byID.Payload) != want {
		t.Errorf("GetByID after update: payload=%q hit=%v want %q", byID.Payload, ok, want)
	}
	if bySeq, ok := g.GetBySeq("s", seq); !ok || string(bySeq.Payload) != want {
		t.Errorf("GetBySeq after update: payload=%q hit=%v want %q", bySeq.Payload, ok, want)
	}
	if items, _, found := g.Tail("s", 10, 0); !found || len(items) != 1 || string(items[0].Payload) != want {
		t.Errorf("Tail after update: items=%+v found=%v want one item with %q", items, found, want)
	}
}

// TestPointerSafety_CounterCoherentAcrossAddressing pins that a
// counter created and incremented via /counter_add reads back the
// same value through every addressing mode.
func TestPointerSafety_CounterCoherentAcrossAddressing(t *testing.T) {
	g := NewGateway(Config{ScopeMaxItems: 100, MaxStoreBytes: 10 << 20, MaxItemBytes: 1 << 20})

	if v, created, err := g.CounterAdd("s", "ctr", 5); err != nil || !created || v != 5 {
		t.Fatalf("counter create: v=%d created=%v err=%v", v, created, err)
	}
	if v, _, err := g.CounterAdd("s", "ctr", 3); err != nil || v != 8 {
		t.Fatalf("counter increment: v=%d err=%v", v, err)
	}

	if byID, ok := g.GetByID("s", "ctr"); !ok || string(byID.Payload) != "8" {
		t.Errorf("GetByID counter: payload=%q hit=%v want \"8\"", byID.Payload, ok)
	}
	// The counter is the only item in the scope, so its seq is 1.
	if bySeq, ok := g.GetBySeq("s", 1); !ok || string(bySeq.Payload) != "8" {
		t.Errorf("GetBySeq counter: payload=%q hit=%v want \"8\"", bySeq.Payload, ok)
	}
	if items, _, found := g.Tail("s", 10, 0); !found || len(items) != 1 || string(items[0].Payload) != "8" {
		t.Errorf("Tail counter: items=%+v found=%v want one item with \"8\"", items, found)
	}
}

// TestPointerSafety_PromoteRollbackOnStoreFull pins the sharpest
// pointer hazard: counterAddSlow's promote builds the counter-shaped
// item, then reserves the byte delta. If the reservation fails the
// ORIGINAL item must be untouched. Aliasing the live item into the
// promote scratch would corrupt it before the reservation check.
func TestPointerSafety_PromoteRollbackOnStoreFull(t *testing.T) {
	g := NewGateway(Config{ScopeMaxItems: 100, MaxStoreBytes: 1 << 20, MaxItemBytes: 1 << 20})
	s := g.store

	if _, err := s.appendOne(Item{Scope: "s", ID: "c", Payload: json.RawMessage(`5`)}); err != nil {
		t.Fatalf("seed append: %v", err)
	}

	// Drive the store byte counter to its cap so the promote's
	// reservation is forced to fail.
	_, current, max := s.reserveBytes(0)
	if ok, _, _ := s.reserveBytes(max - current); !ok {
		t.Fatalf("setup: could not consume the remaining byte budget")
	}

	if _, _, err := s.counterAddOne("s", "c", 1); err == nil {
		t.Fatal("counterAddOne promote: expected a store-full error, got nil")
	}

	// The original item must be completely intact — still a regular
	// item with payload `5`, not a half-promoted counter.
	item, hit := s.get("s", "c", 0)
	if !hit {
		t.Fatal("item gone after a failed promote")
	}
	if string(item.Payload) != "5" {
		t.Errorf("failed promote corrupted the item: payload=%q want \"5\"", item.Payload)
	}

	// Release the artificial reservation; a real promote must now
	// succeed and read the still-intact `5` as its base value.
	s.reserveBytes(-(max - current))
	if v, _, err := s.counterAddOne("s", "c", 1); err != nil || v != 6 {
		t.Errorf("promote after freeing space: v=%d err=%v want v=6 (5+1)", v, err)
	}
}
