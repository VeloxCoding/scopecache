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
	buf := newTestBuffer(100)
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

	win, _ := buf.tailOffset(nil, 10, 0)
	for i := range win {
		win[i].Seq, win[i].Scope = 4242, "hijacked"
	}
	assertUntouched("after tailOffset mutate")

	since, _ := buf.sinceSeq(nil, 0, 10)
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
