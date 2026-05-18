package scopecache

import (
	"encoding/json"
	"testing"
)

// Step 4: uuid as a third single-item lookup key alongside id and seq
// — /get?uuid=, /render?uuid=, and the Gateway GetByUUID/RenderByUUID.

func TestGet_ByUUID_Hit(t *testing.T) {
	h, _ := newTestHandler(10)
	_, appendOut, _ := doRequest(t, h, "POST", "/append", `{"scope":"s","id":"a","payload":{"v":1}}`)
	u := mintedUUID(t, appendOut)

	_, out, _ := doRequest(t, h, "GET", "/get?scope=s&uuid="+u, "")
	item, ok := out["item"].(map[string]interface{})
	if !ok {
		t.Fatalf("no item in /get?uuid= response: %+v", out)
	}
	if item["uuid"] != u {
		t.Fatalf("uuid lookup returned uuid=%v want %s", item["uuid"], u)
	}
	if item["id"] != "a" {
		t.Fatalf("uuid lookup returned id=%v want a", item["id"])
	}
}

func TestGet_ByUUID_Miss(t *testing.T) {
	h, _ := newTestHandler(10)
	_, _, _ = doRequest(t, h, "POST", "/append", `{"scope":"s","payload":{"v":1}}`)
	// A valid v7 string never minted into this cache — a clean miss.
	code, out, _ := doRequest(t, h, "GET", "/get?scope=s&uuid="+sampleUUIDv7, "")
	if code != 200 {
		t.Fatalf("code=%d want 200", code)
	}
	if hit, _ := out["hit"].(bool); hit {
		t.Fatalf("expected hit=false for an absent uuid: %+v", out)
	}
}

func TestGet_ByUUID_MalformedRejected(t *testing.T) {
	h, _ := newTestHandler(10)
	code, _, _ := doRequest(t, h, "GET", "/get?scope=s&uuid=not-a-uuid", "")
	if code != 400 {
		t.Fatalf("code=%d want 400 for a malformed uuid", code)
	}
}

func TestGet_RejectsMultipleAddressingKeys(t *testing.T) {
	h, _ := newTestHandler(10)
	code, _, _ := doRequest(t, h, "GET", "/get?scope=s&id=a&uuid="+sampleUUIDv7, "")
	if code != 400 {
		t.Fatalf("code=%d want 400 (id and uuid both present)", code)
	}
}

func TestGet_RejectsNoAddressingKey(t *testing.T) {
	h, _ := newTestHandler(10)
	code, _, _ := doRequest(t, h, "GET", "/get?scope=s", "")
	if code != 400 {
		t.Fatalf("code=%d want 400 (no id/seq/uuid)", code)
	}
}

func TestRender_ByUUID(t *testing.T) {
	h, _ := newTestHandler(10)
	_, appendOut, _ := doRequest(t, h, "POST", "/append", `{"scope":"s","id":"a","payload":"<h1>hi</h1>"}`)
	u := mintedUUID(t, appendOut)

	code, _, raw := doRequest(t, h, "GET", "/render?scope=s&uuid="+u, "")
	if code != 200 {
		t.Fatalf("code=%d want 200", code)
	}
	if raw != "<h1>hi</h1>" {
		t.Fatalf("/render?uuid= body=%q want %q", raw, "<h1>hi</h1>")
	}
}

func TestGateway_GetAndRenderByUUID(t *testing.T) {
	g := NewGateway(Config{})
	committed, err := g.Append(Item{Scope: "s", ID: "a", Payload: json.RawMessage(`"<h1>hi</h1>"`)})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	u := committed.UUID
	if !isValidUUIDv7(u) {
		t.Fatalf("Append returned no uuid: %q", u)
	}

	item, hit := g.GetByUUID("s", u)
	if !hit {
		t.Fatal("GetByUUID miss")
	}
	if item.ID != "a" {
		t.Fatalf("GetByUUID wrong item: id=%q want a", item.ID)
	}

	body, hit := g.RenderByUUID("s", u)
	if !hit {
		t.Fatal("RenderByUUID miss")
	}
	if string(body) != "<h1>hi</h1>" {
		t.Fatalf("RenderByUUID body=%q want %q", body, "<h1>hi</h1>")
	}

	if _, hit := g.GetByUUID("s", sampleUUIDv7); hit {
		t.Fatal("GetByUUID hit on an absent uuid")
	}
}
