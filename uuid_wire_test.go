package scopecache

import (
	"strings"
	"testing"
)

// uuid is cache-minted: clients cannot supply it on a create or
// replace write — the validator rejects a non-empty Item.UUID with
// 400, same contract as ts. These tests pin step 2: rejection on
// input + the uuid key present on every serialised item.

const sampleUUIDv7 = "01234567-89ab-7cde-8f01-23456789abcd"

// --- Rejection: client-supplied uuid is forbidden ----------------------------

func TestAppend_RejectsClientSuppliedUUID(t *testing.T) {
	h, _ := newTestHandler(10)
	code, out, _ := doRequest(t, h, "POST", "/append",
		`{"scope":"s","id":"a","uuid":"`+sampleUUIDv7+`","payload":{"v":1}}`)
	if code != 400 {
		t.Fatalf("code=%d want 400 (client-supplied uuid must be rejected)", code)
	}
	if errMsg, _ := out["error"].(string); errMsg == "" {
		t.Errorf("expected an error message, got: %+v", out)
	}
}

func TestUpsert_RejectsClientSuppliedUUID(t *testing.T) {
	h, _ := newTestHandler(10)
	code, _, _ := doRequest(t, h, "POST", "/upsert",
		`{"scope":"s","id":"a","uuid":"`+sampleUUIDv7+`","payload":{"v":1}}`)
	if code != 400 {
		t.Fatalf("code=%d want 400", code)
	}
}

func TestUpdate_RejectsClientSuppliedUUID(t *testing.T) {
	h, _ := newTestHandler(10)
	_, _, _ = doRequest(t, h, "POST", "/append", `{"scope":"s","id":"a","payload":{"v":1}}`)
	code, _, _ := doRequest(t, h, "POST", "/update",
		`{"scope":"s","id":"a","uuid":"`+sampleUUIDv7+`","payload":{"v":2}}`)
	if code != 400 {
		t.Fatalf("code=%d want 400", code)
	}
}

// --- uuid key is present on every serialised item ----------------------------

func TestItem_MarshalJSONIncludesUUIDKey(t *testing.T) {
	b, err := Item{Scope: "s", ID: "a", Seq: 1, Ts: 42, UUID: sampleUUIDv7,
		Payload: []byte(`{"v":1}`)}.MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	if !strings.Contains(string(b), `"uuid":"`+sampleUUIDv7+`"`) {
		t.Fatalf("MarshalJSON missing uuid key: %s", b)
	}
	// AppendItemJSON must stay byte-identical.
	if got := string(AppendItemJSON(nil, Item{Scope: "s", ID: "a", Seq: 1, Ts: 42,
		UUID: sampleUUIDv7, Payload: []byte(`{"v":1}`)})); got != string(b) {
		t.Fatalf("AppendItemJSON != MarshalJSON:\n  %s\n  %s", got, b)
	}
}

func TestGetResponse_CarriesUUIDKey(t *testing.T) {
	h, _ := newTestHandler(10)
	_, _, _ = doRequest(t, h, "POST", "/append", `{"scope":"s","id":"a","payload":{"v":1}}`)
	_, out, _ := doRequest(t, h, "GET", "/get?scope=s&id=a", "")
	item, ok := out["item"].(map[string]interface{})
	if !ok {
		t.Fatalf("no item in /get response: %+v", out)
	}
	if _, present := item["uuid"]; !present {
		t.Fatalf("/get item missing uuid key: %+v", item)
	}
}
