package scopecache

import (
	"net/http"
	"testing"
)

// Step 7: an `_events`-scope entry exposes its own minted uuid as
// `event_uuid` (outer) while the inner event envelope carries the
// user-item's uuid as `uuid`.

func eventsFullHandler(t *testing.T) http.Handler {
	t.Helper()
	api := NewAPI(NewGateway(Config{
		ScopeMaxItems: 100, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20,
		Events: EventsConfig{Mode: EventsModeFull},
	}), APIConfig{})
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)
	return mux
}

// lastEventEntry tails `_events` and returns the newest entry.
func lastEventEntry(t *testing.T, h http.Handler) map[string]interface{} {
	t.Helper()
	_, out, _ := doRequest(t, h, "GET", "/tail?scope=_events&limit=50", "")
	items, _ := out["items"].([]interface{})
	if len(items) == 0 {
		t.Fatalf("no _events entries: %+v", out)
	}
	return items[len(items)-1].(map[string]interface{})
}

func TestEvents_AppendCarriesUUIDAndEventUUID(t *testing.T) {
	h := eventsFullHandler(t)
	_, ao, _ := doRequest(t, h, "POST", "/append", `{"scope":"posts","id":"p1","payload":{"v":1}}`)
	itemUUID := mintedUUID(t, ao)

	ev := lastEventEntry(t, h)
	// Outer entry: the _events item is keyed by event_uuid, not uuid.
	if _, hasUUID := ev["uuid"]; hasUUID {
		t.Errorf("_events item exposes 'uuid'; want only 'event_uuid'")
	}
	euuid, ok := ev["event_uuid"].(string)
	if !ok || !isValidUUIDv7(euuid) {
		t.Fatalf("_events item event_uuid missing/invalid: %+v", ev)
	}
	// Inner envelope: carries the appended item's uuid + op.
	env, ok := ev["event"].(map[string]interface{})
	if !ok {
		t.Fatalf("_events item has no 'event' envelope: %+v", ev)
	}
	if env["op"] != "append" {
		t.Fatalf("event op=%v want append", env["op"])
	}
	if env["uuid"] != itemUUID {
		t.Fatalf("event envelope uuid=%v want %s (the appended item's uuid)", env["uuid"], itemUUID)
	}
	// The two uuids are distinct identities.
	if euuid == itemUUID {
		t.Fatal("event_uuid equals the inner item uuid; they must be distinct")
	}
}

func TestEvents_UpsertCarriesItemUUID(t *testing.T) {
	h := eventsFullHandler(t)
	_, uo, _ := doRequest(t, h, "POST", "/upsert", `{"scope":"posts","id":"p1","payload":{"v":1}}`)
	itemUUID := mintedUUID(t, uo)

	ev := lastEventEntry(t, h)
	env, ok := ev["event"].(map[string]interface{})
	if !ok || env["op"] != "upsert" {
		t.Fatalf("expected an upsert event envelope: %+v", ev)
	}
	if env["uuid"] != itemUUID {
		t.Fatalf("upsert event uuid=%v want %s", env["uuid"], itemUUID)
	}
}
