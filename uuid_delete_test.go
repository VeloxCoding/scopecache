package scopecache

import "testing"

// Step 6: uuid as a third addressing key on /update, /delete and
// /delete_up_to (the uuid form of /delete_up_to names the boundary
// item; the store resolves it to that item's seq).

func TestDelete_ByUUID(t *testing.T) {
	h, _ := newTestHandler(10)
	_, ao, _ := doRequest(t, h, "POST", "/append", `{"scope":"s","id":"a","payload":{"v":1}}`)
	u := mintedUUID(t, ao)
	_, _, _ = doRequest(t, h, "POST", "/append", `{"scope":"s","id":"b","payload":{"v":2}}`)

	code, out, _ := doRequest(t, h, "POST", "/delete", `{"scope":"s","uuid":"`+u+`"}`)
	if code != 200 || !mustBool(t, out, "hit") {
		t.Fatalf("delete by uuid: code=%d out=%+v", code, out)
	}
	if _, g, _ := doRequest(t, h, "GET", "/get?scope=s&id=a", ""); mustBool(t, g, "hit") {
		t.Fatal("item a still present after delete by uuid")
	}
	if _, g, _ := doRequest(t, h, "GET", "/get?scope=s&id=b", ""); !mustBool(t, g, "hit") {
		t.Fatal("item b wrongly deleted")
	}
}

func TestDelete_ByUUID_Miss(t *testing.T) {
	h, _ := newTestHandler(10)
	_, _, _ = doRequest(t, h, "POST", "/append", `{"scope":"s","payload":{"v":1}}`)
	code, out, _ := doRequest(t, h, "POST", "/delete", `{"scope":"s","uuid":"`+sampleUUIDv7+`"}`)
	if code != 200 {
		t.Fatalf("code=%d", code)
	}
	if mustBool(t, out, "hit") {
		t.Fatal("delete by an absent uuid reported hit")
	}
}

func TestDelete_RejectsMultipleAddressing(t *testing.T) {
	h, _ := newTestHandler(10)
	code, _, _ := doRequest(t, h, "POST", "/delete", `{"scope":"s","id":"a","uuid":"`+sampleUUIDv7+`"}`)
	if code != 400 {
		t.Fatalf("id+uuid both present: code=%d want 400", code)
	}
}

func TestUpdate_ByUUID(t *testing.T) {
	h, _ := newTestHandler(10)
	_, ao, _ := doRequest(t, h, "POST", "/append", `{"scope":"s","id":"a","payload":{"v":1}}`)
	u := mintedUUID(t, ao)

	if code, _, _ := doRequest(t, h, "POST", "/update", `{"scope":"s","uuid":"`+u+`","payload":{"v":99}}`); code != 200 {
		t.Fatalf("update by uuid: code=%d", code)
	}
	_, g, _ := doRequest(t, h, "GET", "/get?scope=s&id=a", "")
	item := g["item"].(map[string]interface{})
	if pl, _ := item["payload"].(map[string]interface{}); pl["v"].(float64) != 99 {
		t.Fatalf("update by uuid did not apply: payload=%v", item["payload"])
	}
	if item["uuid"] != u {
		t.Fatalf("uuid changed after update by uuid: %v want %s", item["uuid"], u)
	}
}

func TestDeleteUpTo_ByUUID(t *testing.T) {
	h, _ := newTestHandler(100)
	var uuids []string
	for i := 0; i < 5; i++ {
		_, ao, _ := doRequest(t, h, "POST", "/append", `{"scope":"s","payload":{"v":1}}`)
		uuids = append(uuids, mintedUUID(t, ao))
	}
	// Drain up to the 3rd item — items 1,2,3 go, 4,5 stay.
	code, out, _ := doRequest(t, h, "POST", "/delete_up_to", `{"scope":"s","uuid":"`+uuids[2]+`"}`)
	if code != 200 {
		t.Fatalf("delete_up_to by uuid: code=%d", code)
	}
	if c, _ := out["count"].(float64); c != 3 {
		t.Fatalf("delete_up_to by uuid count=%v want 3", out["count"])
	}
	if _, g, _ := doRequest(t, h, "GET", "/get?scope=s&uuid="+uuids[3], ""); !mustBool(t, g, "hit") {
		t.Fatal("item 4 wrongly drained")
	}
}

func TestDeleteUpTo_ByUUID_AbsentBoundaryIsNoOp(t *testing.T) {
	h, _ := newTestHandler(100)
	for i := 0; i < 3; i++ {
		_, _, _ = doRequest(t, h, "POST", "/append", `{"scope":"s","payload":{"v":1}}`)
	}
	code, out, _ := doRequest(t, h, "POST", "/delete_up_to", `{"scope":"s","uuid":"`+sampleUUIDv7+`"}`)
	if code != 200 {
		t.Fatalf("code=%d", code)
	}
	if c, _ := out["count"].(float64); c != 0 {
		t.Fatalf("absent-boundary delete_up_to count=%v want 0", out["count"])
	}
}

func TestDeleteUpTo_RejectsBothOrNeither(t *testing.T) {
	h, _ := newTestHandler(10)
	if code, _, _ := doRequest(t, h, "POST", "/delete_up_to", `{"scope":"s"}`); code != 400 {
		t.Fatalf("neither max_seq nor uuid: code=%d want 400", code)
	}
	if code, _, _ := doRequest(t, h, "POST", "/delete_up_to",
		`{"scope":"s","max_seq":3,"uuid":"`+sampleUUIDv7+`"}`); code != 400 {
		t.Fatalf("both max_seq and uuid: code=%d want 400", code)
	}
}
