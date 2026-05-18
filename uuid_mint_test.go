package scopecache

import "testing"

// Step 3: the cache mints a UUIDv7 on every single-item create
// (/append, /upsert-create, /counter_add-create) and preserves it
// across in-place mutation (/update, /upsert-replace, increment).

// mintedUUID extracts response.item.uuid and asserts it is a valid v7.
func mintedUUID(t *testing.T, out map[string]interface{}) string {
	t.Helper()
	item, ok := out["item"].(map[string]interface{})
	if !ok {
		t.Fatalf("no item object in response: %+v", out)
	}
	u, ok := item["uuid"].(string)
	if !ok || u == "" {
		t.Fatalf("response item has no uuid: %+v", item)
	}
	if !isValidUUIDv7(u) {
		t.Fatalf("response uuid %q is not a valid UUIDv7", u)
	}
	return u
}

func TestAppend_MintsUUIDv7(t *testing.T) {
	h, _ := newTestHandler(10)
	_, out, _ := doRequest(t, h, "POST", "/append", `{"scope":"s","id":"a","payload":{"v":1}}`)
	mintedUUID(t, out)
}

func TestUpsert_MintsUUIDv7OnCreate(t *testing.T) {
	h, _ := newTestHandler(10)
	_, out, _ := doRequest(t, h, "POST", "/upsert", `{"scope":"s","id":"a","payload":{"v":1}}`)
	mintedUUID(t, out)
}

func TestInbox_AppendMintsUUID(t *testing.T) {
	h, _ := newTestHandler(10)
	_, out, _ := doRequest(t, h, "POST", "/append", `{"scope":"_inbox","payload":{"v":1}}`)
	mintedUUID(t, out)
}

// The cache mints a random (not monotonic) UUIDv7 per item — the
// contract is uniqueness, not ordering. Every /append must get a
// distinct uuid.
func TestAppend_UUIDsAreDistinct(t *testing.T) {
	h, _ := newTestHandler(1000)
	seen := make(map[string]struct{})
	for i := 0; i < 500; i++ {
		_, out, _ := doRequest(t, h, "POST", "/append", `{"scope":"s","payload":{"v":1}}`)
		u := mintedUUID(t, out)
		if _, dup := seen[u]; dup {
			t.Fatalf("/append #%d minted a duplicate uuid: %q", i, u)
		}
		seen[u] = struct{}{}
	}
}

func TestUUID_PreservedAcrossUpdate(t *testing.T) {
	h, _ := newTestHandler(10)
	_, out, _ := doRequest(t, h, "POST", "/append", `{"scope":"s","id":"a","payload":{"v":1}}`)
	want := mintedUUID(t, out)

	if code, _, _ := doRequest(t, h, "POST", "/update", `{"scope":"s","id":"a","payload":{"v":2}}`); code != 200 {
		t.Fatalf("update: code=%d", code)
	}
	_, getOut, _ := doRequest(t, h, "GET", "/get?scope=s&id=a", "")
	if got := mintedUUID(t, getOut); got != want {
		t.Fatalf("uuid changed across /update: was %q now %q", want, got)
	}
}

func TestUUID_PreservedAcrossUpsertReplace(t *testing.T) {
	h, _ := newTestHandler(10)
	_, out1, _ := doRequest(t, h, "POST", "/upsert", `{"scope":"s","id":"a","payload":{"v":1}}`)
	want := mintedUUID(t, out1)
	_, out2, _ := doRequest(t, h, "POST", "/upsert", `{"scope":"s","id":"a","payload":{"v":2}}`)
	if got := mintedUUID(t, out2); got != want {
		t.Fatalf("uuid changed across /upsert replace: was %q now %q", want, got)
	}
}

func TestCounterAdd_MintsAndPreservesUUID(t *testing.T) {
	h, _ := newTestHandler(10)
	_, _, _ = doRequest(t, h, "POST", "/counter_add", `{"scope":"s","id":"c","by":5}`)
	_, get1, _ := doRequest(t, h, "GET", "/get?scope=s&id=c", "")
	want := mintedUUID(t, get1)

	_, _, _ = doRequest(t, h, "POST", "/counter_add", `{"scope":"s","id":"c","by":3}`)
	_, get2, _ := doRequest(t, h, "GET", "/get?scope=s&id=c", "")
	if got := mintedUUID(t, get2); got != want {
		t.Fatalf("counter uuid changed across increment: was %q now %q", want, got)
	}
}

// Buffer-level: firstUUID pins the oldest insert, lastUUID tracks the
// newest, byUUID indexes live items, and the span survives a delete.
func TestScopeBuffer_FirstLastUUIDAndIndex(t *testing.T) {
	s := newStore(Config{})
	buf, _ := s.getOrCreateScope("s")

	var uuids []string
	for i := 0; i < 3; i++ {
		it, err := buf.appendItem(newItem("s", "", map[string]interface{}{"v": i}))
		if err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
		if !isValidUUIDv7(it.UUID) {
			t.Fatalf("append %d minted invalid uuid %q", i, it.UUID)
		}
		uuids = append(uuids, it.UUID)
	}
	if buf.firstUUID != uuids[0] {
		t.Fatalf("firstUUID=%q want %q", buf.firstUUID, uuids[0])
	}
	if buf.lastUUID != uuids[2] {
		t.Fatalf("lastUUID=%q want %q", buf.lastUUID, uuids[2])
	}

	// Delete the oldest item — firstUUID must NOT regress (it survives
	// like lastSeq), but byUUID drops the dead entry.
	if _, err := buf.deleteBySeq(1); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if buf.firstUUID != uuids[0] {
		t.Fatalf("firstUUID changed after delete: %q want %q", buf.firstUUID, uuids[0])
	}
	if _, ok := buf.byUUID[uuids[0]]; ok {
		t.Fatalf("byUUID still maps the deleted item's uuid %q", uuids[0])
	}
	if _, ok := buf.byUUID[uuids[2]]; !ok {
		t.Fatalf("byUUID lost a live item's uuid %q", uuids[2])
	}
}
