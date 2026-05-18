package scopecache

import (
	"encoding/json"
	"testing"
)

// Step 5: /warm and /rebuild adopt a client-supplied UUIDv7 (strict v7
// validation, within-scope duplicate rejected) or mint one when absent.

func TestWarm_MintsUUIDWhenAbsent(t *testing.T) {
	h, _ := newTestHandler(100)
	body := `{"items":[` +
		`{"scope":"s","id":"a","payload":{"v":1}},` +
		`{"scope":"s","id":"b","payload":{"v":2}}]}`
	if code, _, raw := doAdminRequest(t, h, "/warm", body); code != 200 {
		t.Fatalf("warm: code=%d body=%s", code, raw)
	}
	for _, id := range []string{"a", "b"} {
		_, out, _ := doRequest(t, h, "GET", "/get?scope=s&id="+id, "")
		mintedUUID(t, out) // asserts a valid v7 uuid is present
	}
}

func TestWarm_AdoptsClientUUID(t *testing.T) {
	h, _ := newTestHandler(100)
	body := `{"items":[{"scope":"s","id":"a","uuid":"` + sampleUUIDv7 + `","payload":{"v":1}}]}`
	if code, _, raw := doAdminRequest(t, h, "/warm", body); code != 200 {
		t.Fatalf("warm: code=%d body=%s", code, raw)
	}
	// The adopted uuid must be the live lookup key.
	_, out, _ := doRequest(t, h, "GET", "/get?scope=s&uuid="+sampleUUIDv7, "")
	item, ok := out["item"].(map[string]interface{})
	if !ok || item["uuid"] != sampleUUIDv7 {
		t.Fatalf("adopted uuid not found via /get?uuid=: %+v", out)
	}
}

func TestWarm_MixedMintAndAdopt(t *testing.T) {
	h, _ := newTestHandler(100)
	body := `{"items":[` +
		`{"scope":"s","id":"adopted","uuid":"` + sampleUUIDv7 + `","payload":{"v":1}},` +
		`{"scope":"s","id":"minted","payload":{"v":2}}]}`
	if code, _, raw := doAdminRequest(t, h, "/warm", body); code != 200 {
		t.Fatalf("warm: code=%d body=%s", code, raw)
	}
	_, out, _ := doRequest(t, h, "GET", "/get?scope=s&id=adopted", "")
	if item, _ := out["item"].(map[string]interface{}); item["uuid"] != sampleUUIDv7 {
		t.Fatalf("adopted item uuid=%v want %s", item["uuid"], sampleUUIDv7)
	}
	_, out2, _ := doRequest(t, h, "GET", "/get?scope=s&id=minted", "")
	if u := mintedUUID(t, out2); u == sampleUUIDv7 {
		t.Fatal("minted item collided with the adopted uuid")
	}
}

func TestWarm_RejectsNonV7UUID(t *testing.T) {
	h, _ := newTestHandler(100)
	// version 4, not 7.
	body := `{"items":[{"scope":"s","id":"a","uuid":"01234567-89ab-4cde-8f01-23456789abcd","payload":{"v":1}}]}`
	if code, _, _ := doAdminRequest(t, h, "/warm", body); code != 400 {
		t.Fatalf("warm with a non-v7 uuid: code=%d want 400", code)
	}
}

func TestWarm_RejectsDuplicateUUIDInScope(t *testing.T) {
	h, _ := newTestHandler(100)
	body := `{"items":[` +
		`{"scope":"s","id":"a","uuid":"` + sampleUUIDv7 + `","payload":{"v":1}},` +
		`{"scope":"s","id":"b","uuid":"` + sampleUUIDv7 + `","payload":{"v":2}}]}`
	if code, _, _ := doAdminRequest(t, h, "/warm", body); code != 400 {
		t.Fatalf("warm with a duplicate uuid: code=%d want 400", code)
	}
}

func TestRebuild_MintsAndAdoptsUUID(t *testing.T) {
	h, _ := newTestHandler(100)
	body := `{"items":[` +
		`{"scope":"s","id":"adopted","uuid":"` + sampleUUIDv7 + `","payload":{"v":1}},` +
		`{"scope":"s","id":"minted","payload":{"v":2}}]}`
	if code, _, raw := doAdminRequest(t, h, "/rebuild", body); code != 200 {
		t.Fatalf("rebuild: code=%d body=%s", code, raw)
	}
	_, out, _ := doRequest(t, h, "GET", "/get?scope=s&uuid="+sampleUUIDv7, "")
	if item, _ := out["item"].(map[string]interface{}); item["id"] != "adopted" {
		t.Fatalf("rebuild did not adopt the uuid: %+v", out)
	}
	_, out2, _ := doRequest(t, h, "GET", "/get?scope=s&id=minted", "")
	mintedUUID(t, out2)
}

// Store-level: /warm sets the firstUUID/lastUUID span and a complete
// byUUID index on the replaced scope.
func TestWarm_SetsFirstLastUUIDAndIndex(t *testing.T) {
	s := newStore(Config{})
	grouped := map[string][]Item{
		"s": {
			{Scope: "s", ID: "a", Payload: json.RawMessage(`{"v":1}`)},
			{Scope: "s", ID: "b", Payload: json.RawMessage(`{"v":2}`)},
			{Scope: "s", ID: "c", Payload: json.RawMessage(`{"v":3}`)},
		},
	}
	if _, err := s.replaceScopes(grouped); err != nil {
		t.Fatalf("replaceScopes: %v", err)
	}
	buf, ok := s.getScope("s")
	if !ok {
		t.Fatal("scope missing after warm")
	}
	if !isValidUUIDv7(buf.firstUUID) || !isValidUUIDv7(buf.lastUUID) {
		t.Fatalf("firstUUID/lastUUID not set: first=%q last=%q", buf.firstUUID, buf.lastUUID)
	}
	if buf.firstUUID != buf.items[0].UUID || buf.lastUUID != buf.items[2].UUID {
		t.Fatalf("uuid span mismatch: first=%q last=%q items=[%q..%q]",
			buf.firstUUID, buf.lastUUID, buf.items[0].UUID, buf.items[2].UUID)
	}
	if len(buf.byUUID) != 3 {
		t.Fatalf("byUUID has %d entries want 3", len(buf.byUUID))
	}
}
