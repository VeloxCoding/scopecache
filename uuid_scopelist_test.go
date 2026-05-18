package scopecache

import "testing"

// Step 8: /scopelist detail rows carry first_uuid / last_uuid — the
// scope's UUIDv7 span.

func TestScopeList_CarriesFirstLastUUID(t *testing.T) {
	h, _ := newTestHandler(100)
	var uuids []string
	for i := 0; i < 3; i++ {
		_, ao, _ := doRequest(t, h, "POST", "/append", `{"scope":"posts","payload":{"v":1}}`)
		uuids = append(uuids, mintedUUID(t, ao))
	}

	_, out, _ := doRequest(t, h, "GET", "/scopelist", "")
	scopes, _ := out["scopes"].([]interface{})
	var entry map[string]interface{}
	for _, s := range scopes {
		m := s.(map[string]interface{})
		if m["scope"] == "posts" {
			entry = m
		}
	}
	if entry == nil {
		t.Fatalf("'posts' not in /scopelist: %+v", out)
	}
	if entry["first_uuid"] != uuids[0] {
		t.Fatalf("first_uuid=%v want %s", entry["first_uuid"], uuids[0])
	}
	if entry["last_uuid"] != uuids[2] {
		t.Fatalf("last_uuid=%v want %s", entry["last_uuid"], uuids[2])
	}
}

func TestScopeList_NeverWrittenScopeHasEmptyUUIDSpan(t *testing.T) {
	h, _ := newTestHandler(100)
	// _inbox is pre-created at boot and never written here.
	_, out, _ := doRequest(t, h, "GET", "/scopelist", "")
	scopes, _ := out["scopes"].([]interface{})
	for _, s := range scopes {
		m := s.(map[string]interface{})
		if m["scope"] != InboxScopeName {
			continue
		}
		fu, hasFirst := m["first_uuid"]
		lu, hasLast := m["last_uuid"]
		if !hasFirst || !hasLast {
			t.Fatalf("_inbox row missing uuid-span keys: %+v", m)
		}
		if fu != "" || lu != "" {
			t.Fatalf("_inbox never written but span non-empty: first=%v last=%v", fu, lu)
		}
		return
	}
	t.Fatal("_inbox not in /scopelist")
}
