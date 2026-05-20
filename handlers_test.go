package scopecache

import (
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func newTestHandler(maxItems int) (http.Handler, *API) {
	// 100 MiB byte budget is more than enough for handler tests with tiny
	// payloads; dedicated byte-cap behaviour tests construct stores with a
	// small maxStoreBytes so their writes can fail the store cap on purpose.
	api := NewAPI(
		NewGateway(Config{ScopeMaxItems: maxItems, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20}),
		APIConfig{},
	)
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)
	return mux, api
}

func doRequest(t *testing.T, h http.Handler, method, path, body string) (int, map[string]interface{}, string) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	if r != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	raw := rec.Body.String()
	var out map[string]interface{}
	if strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		_ = json.Unmarshal([]byte(raw), &out)
	}
	return rec.Code, out, raw
}

// doAdminRequest is a thin compatibility shim left over from when /wipe,
// /warm, /rebuild, /delete_scope, /stats lived behind /admin's envelope.
// Those endpoints are now public on the mux (no admin gate, no envelope —
// see core-and-addons.md), so this helper just dispatches directly with
// the right HTTP method. Kept rather than renamed so existing test
// callers stay unchanged during the refactor; new tests should call
// doRequest directly.
func doAdminRequest(t *testing.T, h http.Handler, path, body string) (int, map[string]interface{}, string) {
	t.Helper()
	method := http.MethodPost
	if path == "/stats" {
		method = http.MethodGet
	}
	return doRequest(t, h, method, path, body)
}

func mustBool(t *testing.T, m map[string]interface{}, key string) bool {
	t.Helper()
	v, ok := m[key]
	if !ok {
		t.Fatalf("missing key %q in response: %+v", key, m)
	}
	b, ok := v.(bool)
	if !ok {
		t.Fatalf("key %q is not bool: %v", key, v)
	}
	return b
}

func mustFloat(t *testing.T, m map[string]interface{}, key string) float64 {
	t.Helper()
	v, ok := m[key]
	if !ok {
		t.Fatalf("missing key %q in response: %+v", key, m)
	}
	n, ok := v.(float64)
	if !ok {
		t.Fatalf("key %q is not a number: %v", key, v)
	}
	return n
}

// --- /help --------------------------------------------------------------------

func TestHelp_GETReturnsText(t *testing.T) {
	h, _ := newTestHandler(10)
	req := httptest.NewRequest(http.MethodGet, "/help", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "scopecache") {
		t.Fatal("help body missing 'scopecache'")
	}
}

func TestHelp_POSTRejected(t *testing.T) {
	h, _ := newTestHandler(10)
	req := httptest.NewRequest(http.MethodPost, "/help", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("code=%d want 405", rec.Code)
	}
}

// --- /append ------------------------------------------------------------------

func TestAppend_Success(t *testing.T) {
	h, _ := newTestHandler(10)
	code, out, _ := doRequest(t, h, "POST", "/append", `{"scope":"s","id":"a","payload":{"v":1}}`)

	if code != 200 {
		t.Fatalf("code=%d want 200", code)
	}
	if !mustBool(t, out, "ok") {
		t.Fatal("ok=false")
	}
	item, ok := out["item"].(map[string]interface{})
	if !ok {
		t.Fatalf("item not object: %v", out["item"])
	}
	if item["seq"].(float64) != 1 {
		t.Errorf("seq=%v want 1", item["seq"])
	}
	// ts is cache-owned: every item carries one after a write. Detailed
	// ts behaviour is exercised in ts_test.go.
	if _, hasTS := item["ts"]; !hasTS {
		t.Errorf("response item must carry a cache-assigned 'ts' field: %v", item)
	}
	// /append response echoes scope/id/seq/ts under "item", but NOT
	// payload — the client supplied that on the way in. This pin
	// catches a regression that would re-introduce the payload echo
	// (doubling wire cost on large writes).
	if _, present := item["payload"]; present {
		t.Errorf("/append response item must not echo payload back: %v", item)
	}
}

// Drives a 200-byte scope and 200-byte id end-to-end: over the pre-v0.5.11
// cap of 128 but under the new 256 cap. Both must be accepted by the
// validator and round-trip through /get unchanged. Anchors the "long
// scope/id keys reach the public mux without being truncated or
// rejected" property.
func TestAppend_Acceps200ByteScopeAndID(t *testing.T) {
	h, _ := newTestHandler(10)
	longScope := strings.Repeat("s", 200)
	longID := strings.Repeat("i", 200)

	body := fmt.Sprintf(`{"scope":%q,"id":%q,"payload":{"v":1}}`, longScope, longID)
	if code, _, raw := doRequest(t, h, "POST", "/append", body); code != 200 {
		t.Fatalf("append 200/200: code=%d body=%s", code, raw)
	}

	getURL := fmt.Sprintf("/get?scope=%s&id=%s", longScope, longID)
	code, out, raw := doRequest(t, h, "GET", getURL, "")
	if code != 200 {
		t.Fatalf("get: code=%d body=%s", code, raw)
	}
	if !mustBool(t, out, "hit") {
		t.Errorf("get returned hit=false: %s", raw)
	}
	item := out["item"].(map[string]interface{})
	if item["scope"].(string) != longScope {
		t.Errorf("scope round-trip mismatch")
	}
	if item["id"].(string) != longID {
		t.Errorf("id round-trip mismatch")
	}
}

func TestAppend_MissingPayload(t *testing.T) {
	h, _ := newTestHandler(10)
	code, out, _ := doRequest(t, h, "POST", "/append", `{"scope":"s","id":"a"}`)
	if code != 400 {
		t.Fatalf("code=%d want 400", code)
	}
	if mustBool(t, out, "ok") {
		t.Error("ok=true for invalid request")
	}
}

func TestAppend_MissingScope(t *testing.T) {
	h, _ := newTestHandler(10)
	code, _, _ := doRequest(t, h, "POST", "/append", `{"id":"a","payload":{"v":1}}`)
	if code != 400 {
		t.Fatalf("code=%d want 400", code)
	}
}

func TestAppend_SeqForbidden(t *testing.T) {
	h, _ := newTestHandler(10)
	code, _, _ := doRequest(t, h, "POST", "/append", `{"scope":"s","seq":5,"payload":{"v":1}}`)
	if code != 400 {
		t.Fatalf("code=%d want 400", code)
	}
}

func TestAppend_DuplicateIDReturns409(t *testing.T) {
	h, _ := newTestHandler(10)
	_, _, _ = doRequest(t, h, "POST", "/append", `{"scope":"s","id":"a","payload":{"v":1}}`)
	code, _, _ := doRequest(t, h, "POST", "/append", `{"scope":"s","id":"a","payload":{"v":2}}`)
	if code != 409 {
		t.Fatalf("code=%d want 409", code)
	}
}

func TestAppend_InvalidJSON(t *testing.T) {
	h, _ := newTestHandler(10)
	code, _, _ := doRequest(t, h, "POST", "/append", `not json`)
	if code != 400 {
		t.Fatalf("code=%d want 400", code)
	}
}

// Guards decodeBody against a json.Decoder quirk: without a trailing-EOF
// check, a body containing two concatenated JSON values (or one value plus
// garbage) would silently decode the first and ignore the rest.
func TestAppend_RejectsTrailingContent(t *testing.T) {
	cases := map[string]string{
		"two objects":      `{"scope":"x","payload":{"v":1}}{"scope":"y","payload":{"v":2}}`,
		"trailing garbage": `{"scope":"x","payload":{"v":1}} garbage`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			h, api := newTestHandler(10)
			code, _, _ := doRequest(t, h, "POST", "/append", body)
			if code != 400 {
				t.Fatalf("code=%d want 400", code)
			}
			if _, ok := api.store.getScope("x"); ok {
				t.Fatalf("scope 'x' must not exist: the first value must not be committed")
			}
		})
	}
}

func TestAppend_MethodNotAllowed(t *testing.T) {
	h, _ := newTestHandler(10)
	code, _, _ := doRequest(t, h, "GET", "/append", "")
	if code != http.StatusMethodNotAllowed {
		t.Fatalf("code=%d want 405", code)
	}
}

// RFC 7231 §7.4.1: 405 responses MUST carry an Allow header naming
// the supported method(s). One spot-check per route family — the
// helper is shared, so coverage on one POST + one GET endpoint
// pins the contract for every other handler that uses
// methodNotAllowed.
func TestMethodNotAllowed_SetsAllowHeader(t *testing.T) {
	h, _ := newTestHandler(10)

	cases := []struct {
		path        string
		badMethod   string
		wantAllowed string
	}{
		{"/append", "GET", "POST"},
		{"/get", "POST", "GET"},
	}
	for _, c := range cases {
		rr := doRawRequest(t, h, c.badMethod, c.path)
		if rr.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s: code=%d want 405", c.badMethod, c.path, rr.Code)
			continue
		}
		if got := rr.Header().Get("Allow"); got != c.wantAllowed {
			t.Errorf("%s %s: Allow=%q want %q", c.badMethod, c.path, got, c.wantAllowed)
		}
	}
}

// --- /warm --------------------------------------------------------------------

func TestWarm_LeavesOtherScopesUntouched(t *testing.T) {
	h, api := newTestHandler(10)

	keep, _ := api.store.getOrCreateScope("keep")
	_, _ = keep.appendItem(newItem("keep", "k1", nil))

	body := `{"items":[
		{"scope":"target","id":"t1","payload":{"v":1}},
		{"scope":"target","id":"t2","payload":{"v":2}}
	]}`
	code, out, _ := doAdminRequest(t, h, "/warm", body)
	if code != 200 {
		t.Fatalf("code=%d want 200", code)
	}
	if mustFloat(t, out, "scopes") != 1 {
		t.Errorf("scopes=%v want 1", out["scopes"])
	}

	kept, _ := api.store.getScope("keep")
	if len(kept.items) != 1 {
		t.Fatalf("untouched scope lost items: %d", len(kept.items))
	}
}

// Duplicate id within the same scope in /warm input is a request-shape
// problem (the same primary-key field appears twice in one batch), not
// a conflict against existing state — so 400 is the right code, not
// 409. Pre-step-6.7 this returned 409 because dup-detection happened
// inside buildReplacementState and the handler defaulted unknown
// errors to 409; post-6.7 the validation wrap promotes it to 400.
func TestWarm_DuplicateIDInSameScope(t *testing.T) {
	h, _ := newTestHandler(10)
	body := `{"items":[
		{"scope":"s","id":"a","payload":{"v":1}},
		{"scope":"s","id":"a","payload":{"v":2}}
	]}`
	code, _, _ := doAdminRequest(t, h, "/warm", body)
	if code != 400 {
		t.Fatalf("code=%d want 400", code)
	}
}

func TestWarm_MissingScopeOnItem(t *testing.T) {
	h, _ := newTestHandler(10)
	body := `{"items":[{"id":"a","payload":{"v":1}}]}`
	code, _, _ := doAdminRequest(t, h, "/warm", body)
	if code != 400 {
		t.Fatalf("code=%d want 400", code)
	}
}

// --- /rebuild -----------------------------------------------------------------

func TestRebuild_WipesExistingScopes(t *testing.T) {
	h, api := newTestHandler(10)

	old, _ := api.store.getOrCreateScope("old")
	_, _ = old.appendItem(newItem("old", "", nil))

	body := `{"items":[{"scope":"new","id":"n1","payload":{"v":1}}]}`
	code, out, _ := doAdminRequest(t, h, "/rebuild", body)
	if code != 200 {
		t.Fatalf("code=%d want 200", code)
	}
	if mustFloat(t, out, "scopes") != 1 {
		t.Errorf("scopes=%v want 1", out["scopes"])
	}

	if _, ok := api.store.getScope("old"); ok {
		t.Fatal("old scope should be wiped")
	}
}

// An empty items[] would wipe the store. That's almost always a client bug,
// so /rebuild rejects it with 400 instead of silently clearing everything.
func TestRebuild_RejectsEmptyItems(t *testing.T) {
	h, api := newTestHandler(10)

	keep, _ := api.store.getOrCreateScope("keep")
	_, _ = keep.appendItem(newItem("keep", "k", nil))

	code, _, _ := doAdminRequest(t, h, "/rebuild", `{"items":[]}`)
	if code != 400 {
		t.Fatalf("code=%d want 400 on empty rebuild", code)
	}
	// Store must be untouched after the rejected rebuild.
	if _, ok := api.store.getScope("keep"); !ok {
		t.Fatal("keep scope was wiped despite rejected rebuild")
	}
}

// --- /update ------------------------------------------------------------------

func TestUpdate_Hit(t *testing.T) {
	h, _ := newTestHandler(10)
	_, _, _ = doRequest(t, h, "POST", "/append", `{"scope":"s","id":"a","payload":{"v":1}}`)

	code, out, _ := doRequest(t, h, "POST", "/update", `{"scope":"s","id":"a","payload":{"v":2}}`)
	if code != 200 {
		t.Fatalf("code=%d want 200", code)
	}
	if mustFloat(t, out, "count") != 1 {
		t.Errorf("count=%v want 1", out["count"])
	}

	_, got, _ := doRequest(t, h, "GET", "/get?scope=s&id=a", "")
	item := got["item"].(map[string]interface{})
	if item["payload"].(map[string]interface{})["v"].(float64) != 2 {
		t.Error("payload not updated")
	}
}

func TestUpdate_MissScope(t *testing.T) {
	h, _ := newTestHandler(10)
	code, out, _ := doRequest(t, h, "POST", "/update", `{"scope":"none","id":"x","payload":{"v":1}}`)
	if code != 200 {
		t.Fatalf("code=%d want 200", code)
	}
	if mustFloat(t, out, "count") != 0 {
		t.Errorf("count=%v want 0 for missing scope", out["count"])
	}
}

func TestUpdate_MissID(t *testing.T) {
	h, _ := newTestHandler(10)
	_, _, _ = doRequest(t, h, "POST", "/append", `{"scope":"s","id":"a","payload":{"v":1}}`)
	code, out, _ := doRequest(t, h, "POST", "/update", `{"scope":"s","id":"zzz","payload":{"v":1}}`)
	if code != 200 {
		t.Fatalf("code=%d want 200", code)
	}
	if mustFloat(t, out, "count") != 0 {
		t.Errorf("count=%v want 0 for missing id", out["count"])
	}
}

func TestUpdate_BySeq_Hit(t *testing.T) {
	h, _ := newTestHandler(10)
	_, _, _ = doRequest(t, h, "POST", "/append", `{"scope":"s","payload":{"v":1}}`)

	code, out, _ := doRequest(t, h, "POST", "/update", `{"scope":"s","seq":1,"payload":{"v":9}}`)
	if code != 200 {
		t.Fatalf("code=%d want 200", code)
	}
	if mustFloat(t, out, "count") != 1 {
		t.Errorf("count=%v want 1", out["count"])
	}

	_, got, _ := doRequest(t, h, "GET", "/get?scope=s&seq=1", "")
	item := got["item"].(map[string]interface{})
	if item["payload"].(map[string]interface{})["v"].(float64) != 9 {
		t.Error("payload not updated")
	}
}

func TestUpdate_BySeq_Miss(t *testing.T) {
	h, _ := newTestHandler(10)
	code, out, _ := doRequest(t, h, "POST", "/update", `{"scope":"s","seq":42,"payload":{"v":1}}`)
	if code != 200 {
		t.Fatalf("code=%d want 200", code)
	}
	if mustFloat(t, out, "count") != 0 {
		t.Errorf("count=%v want 0 for missing seq", out["count"])
	}
}

func TestUpdate_RejectsBothIDAndSeq(t *testing.T) {
	h, _ := newTestHandler(10)
	code, _, _ := doRequest(t, h, "POST", "/update", `{"scope":"s","id":"a","seq":1,"payload":{"v":1}}`)
	if code != 400 {
		t.Fatalf("code=%d want 400", code)
	}
}

func TestUpdate_RejectsNeitherIDNorSeq(t *testing.T) {
	h, _ := newTestHandler(10)
	code, _, _ := doRequest(t, h, "POST", "/update", `{"scope":"s","payload":{"v":1}}`)
	if code != 400 {
		t.Fatalf("code=%d want 400", code)
	}
}

// /update by seq must enforce the same per-item byte cap as /update
// by id. The validator measures size on the request body, where seq
// updates carry an empty ID — so a long-id scope can be addressed
// either way and the validator's check undercounts by len(stored.ID)
// in the seq case. Without the post-load size check in updateBySeq,
// a payload that's rejected via id is silently committed via seq,
// blowing past MaxItemBytes.
//
// Setup picks a payload size that fits under cap with id="" and
// exceeds it with id="abcdefghij" (10 bytes), so the asymmetry is
// the only thing under test.
func TestUpdate_BySeq_EnforcesPerItemCap(t *testing.T) {
	api := NewAPI(
		NewGateway(Config{ScopeMaxItems: 10, MaxStoreBytes: 1 << 20, MaxItemBytes: 100}),
		APIConfig{},
	)
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	// Seed: scope=1B, id=10B, payload=7B → approxItemSize = 48+1+10+7 = 66 ≤ 100
	if code, _, _ := doRequest(t, mux, "POST", "/append",
		`{"scope":"s","id":"abcdefghij","payload":{"v":1}}`); code != 200 {
		t.Fatalf("seed append: code=%d want 200", code)
	}

	// Build a payload that, with the stored id (10B), exceeds the cap:
	// 48+1+10+45 = 104 > 100. Without the stored id, 48+1+0+45 = 94 ≤ 100,
	// so the validator alone does not reject.
	n := 45
	buf := make([]byte, n)
	buf[0] = '['
	for i := 1; i < n-1; i++ {
		if i%2 == 1 {
			buf[i] = '1'
		} else {
			buf[i] = ','
		}
	}
	buf[n-1] = ']'
	bigPayload := string(buf)

	// Sanity: update-by-id must reject (the validator catches it).
	if code, _, _ := doRequest(t, mux, "POST", "/update",
		`{"scope":"s","id":"abcdefghij","payload":`+bigPayload+`}`); code != 400 {
		t.Fatalf("update-by-id: code=%d want 400", code)
	}

	// The fix: update-by-seq must also reject, with the post-load
	// re-check inside updateBySeq.
	if code, _, _ := doRequest(t, mux, "POST", "/update",
		`{"scope":"s","seq":1,"payload":`+bigPayload+`}`); code != 400 {
		t.Fatalf("update-by-seq: code=%d want 400 (regression: seq path bypassed cap)", code)
	}

	// Confirm the original payload is still in place — the rejected
	// update must not have committed.
	_, out, _ := doRequest(t, mux, "GET", "/get?scope=s&seq=1", "")
	item := out["item"].(map[string]interface{})
	if item["payload"].(map[string]interface{})["v"].(float64) != 1 {
		t.Errorf("rejected /update mutated state; payload=%v", item["payload"])
	}
}

// --- /upsert ------------------------------------------------------------------

func TestUpsert_Creates(t *testing.T) {
	h, _ := newTestHandler(10)

	code, out, _ := doRequest(t, h, "POST", "/upsert", `{"scope":"s","id":"a","payload":{"v":1}}`)
	if code != 200 {
		t.Fatalf("code=%d want 200", code)
	}
	if !mustBool(t, out, "created") {
		t.Error("created=false on first upsert")
	}
	item := out["item"].(map[string]interface{})
	if item["seq"].(float64) != 1 {
		t.Errorf("seq=%v want 1", item["seq"])
	}
	if _, present := item["payload"]; present {
		t.Errorf("/upsert response item must not echo payload back: %v", item)
	}

	_, got, _ := doRequest(t, h, "GET", "/get?scope=s&id=a", "")
	if !mustBool(t, got, "hit") {
		t.Error("get after upsert missed")
	}
}

func TestUpsert_Replaces(t *testing.T) {
	h, _ := newTestHandler(10)
	_, _, _ = doRequest(t, h, "POST", "/upsert", `{"scope":"s","id":"a","payload":{"v":1}}`)

	code, out, _ := doRequest(t, h, "POST", "/upsert", `{"scope":"s","id":"a","payload":{"v":2}}`)
	if code != 200 {
		t.Fatalf("code=%d want 200", code)
	}
	if mustBool(t, out, "created") {
		t.Error("created=true on replace")
	}
	item := out["item"].(map[string]interface{})
	if item["seq"].(float64) != 1 {
		t.Errorf("seq=%v want 1 (preserved)", item["seq"])
	}

	_, got, _ := doRequest(t, h, "GET", "/get?scope=s&id=a", "")
	gotItem := got["item"].(map[string]interface{})
	if gotItem["payload"].(map[string]interface{})["v"].(float64) != 2 {
		t.Error("payload not replaced")
	}
}

func TestUpsert_MissingID(t *testing.T) {
	h, _ := newTestHandler(10)
	code, _, _ := doRequest(t, h, "POST", "/upsert", `{"scope":"s","payload":{"v":1}}`)
	if code != 400 {
		t.Fatalf("code=%d want 400", code)
	}
}

func TestUpsert_MissingScope(t *testing.T) {
	h, _ := newTestHandler(10)
	code, _, _ := doRequest(t, h, "POST", "/upsert", `{"id":"a","payload":{"v":1}}`)
	if code != 400 {
		t.Fatalf("code=%d want 400", code)
	}
}

func TestUpsert_MissingPayload(t *testing.T) {
	h, _ := newTestHandler(10)
	code, _, _ := doRequest(t, h, "POST", "/upsert", `{"scope":"s","id":"a"}`)
	if code != 400 {
		t.Fatalf("code=%d want 400", code)
	}
}

func TestUpsert_SeqForbidden(t *testing.T) {
	h, _ := newTestHandler(10)
	code, _, _ := doRequest(t, h, "POST", "/upsert", `{"scope":"s","id":"a","seq":5,"payload":{"v":1}}`)
	if code != 400 {
		t.Fatalf("code=%d want 400", code)
	}
}

func TestUpsert_MethodNotAllowed(t *testing.T) {
	h, _ := newTestHandler(10)
	code, _, _ := doRequest(t, h, "GET", "/upsert", "")
	if code != http.StatusMethodNotAllowed {
		t.Fatalf("code=%d want 405", code)
	}
}

// --- /counter_add -------------------------------------------------------------

func TestCounterAdd_CreatesOnMiss(t *testing.T) {
	h, _ := newTestHandler(10)

	code, out, _ := doRequest(t, h, "POST", "/counter_add", `{"scope":"views","id":"article_1","by":1}`)
	if code != 200 {
		t.Fatalf("code=%d want 200", code)
	}
	if !mustBool(t, out, "created") {
		t.Error("created=false on first call")
	}
	if mustFloat(t, out, "value") != 1 {
		t.Errorf("value=%v want 1", out["value"])
	}

	// Round-trip through /get — payload is a bare JSON number.
	_, got, _ := doRequest(t, h, "GET", "/get?scope=views&id=article_1", "")
	if !mustBool(t, got, "hit") {
		t.Error("round-trip /get miss")
	}
}

func TestCounterAdd_Increments(t *testing.T) {
	h, _ := newTestHandler(10)
	_, _, _ = doRequest(t, h, "POST", "/counter_add", `{"scope":"c","id":"k","by":10}`)

	code, out, _ := doRequest(t, h, "POST", "/counter_add", `{"scope":"c","id":"k","by":5}`)
	if code != 200 {
		t.Fatalf("code=%d want 200", code)
	}
	if mustBool(t, out, "created") {
		t.Error("created=true on existing counter")
	}
	if mustFloat(t, out, "value") != 15 {
		t.Errorf("value=%v want 15", out["value"])
	}
}

func TestCounterAdd_NegativeBy(t *testing.T) {
	h, _ := newTestHandler(10)
	_, _, _ = doRequest(t, h, "POST", "/counter_add", `{"scope":"c","id":"k","by":100}`)

	_, out, _ := doRequest(t, h, "POST", "/counter_add", `{"scope":"c","id":"k","by":-40}`)
	if mustFloat(t, out, "value") != 60 {
		t.Errorf("value=%v want 60", out["value"])
	}
}

func TestCounterAdd_ConflictOnNonNumericExisting(t *testing.T) {
	h, _ := newTestHandler(10)
	// Seed with an HTML-ish string payload via /append.
	_, _, _ = doRequest(t, h, "POST", "/append", `{"scope":"pages","id":"home","payload":"<html/>"}`)

	code, out, _ := doRequest(t, h, "POST", "/counter_add", `{"scope":"pages","id":"home","by":1}`)
	if code != http.StatusConflict {
		t.Fatalf("code=%d want 409", code)
	}
	if mustBool(t, out, "ok") {
		t.Error("ok=true on conflict")
	}
}

func TestCounterAdd_ConflictOnFloatExisting(t *testing.T) {
	h, _ := newTestHandler(10)
	_, _, _ = doRequest(t, h, "POST", "/upsert", `{"scope":"c","id":"k","payload":3.14}`)

	code, _, _ := doRequest(t, h, "POST", "/counter_add", `{"scope":"c","id":"k","by":1}`)
	if code != http.StatusConflict {
		t.Fatalf("code=%d want 409", code)
	}
}

func TestCounterAdd_BadRequestOnZeroBy(t *testing.T) {
	h, _ := newTestHandler(10)
	code, _, _ := doRequest(t, h, "POST", "/counter_add", `{"scope":"c","id":"k","by":0}`)
	if code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400", code)
	}
}

func TestCounterAdd_BadRequestOnMissingBy(t *testing.T) {
	h, _ := newTestHandler(10)
	code, _, _ := doRequest(t, h, "POST", "/counter_add", `{"scope":"c","id":"k"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400", code)
	}
}

func TestCounterAdd_BadRequestOnMissingID(t *testing.T) {
	h, _ := newTestHandler(10)
	code, _, _ := doRequest(t, h, "POST", "/counter_add", `{"scope":"c","by":1}`)
	if code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400", code)
	}
}

// /counter_add must enforce the same per-item byte cap as /append.
// Counter items pay counterCellOverhead (56 B) in place of len(Payload),
// so the candidate counter shape's size is fully determined by
// scope+id+overhead — checkable up-front. Without the validator gate,
// counterAddSlow's create AND promote branches commit oversized items
// against the store-byte cap only.
//
// Setup: cap=64, scope=1B, id=1B → counter candidate = 48+1+1+56 = 106B,
// which is well over cap. /append at the same scope/id with any
// payload fitting the cap stays under by construction (overhead is
// only 48+scope+id+payload). The asymmetry the validator now closes
// is "regular item fits, counter doesn't".
func TestCounterAdd_EnforcesPerItemCap_Create(t *testing.T) {
	api := NewAPI(
		NewGateway(Config{ScopeMaxItems: 10, MaxStoreBytes: 1 << 20, MaxItemBytes: 64}),
		APIConfig{},
	)
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	code, _, raw := doRequest(t, mux, "POST", "/counter_add", `{"scope":"s","id":"x","by":1}`)
	if code != http.StatusBadRequest {
		t.Fatalf("create-path /counter_add: code=%d want 400 (body=%s)", code, raw)
	}

	// And the rejected counter must NOT have created an item.
	_, out, _ := doRequest(t, mux, "GET", "/get?scope=s&id=x", "")
	if mustBool(t, out, "hit") {
		t.Errorf("rejected /counter_add still created an item: %+v", out)
	}
}

// Promote path: a regular int item at (scope, id) gets converted to a
// counter on first /counter_add. The pre-fix counter candidate could
// exceed MaxItemBytes even when the predecessor regular item fit;
// validateCounterAddRequest now rejects up-front. The original item
// must remain untouched.
func TestCounterAdd_EnforcesPerItemCap_Promote(t *testing.T) {
	api := NewAPI(
		NewGateway(Config{ScopeMaxItems: 10, MaxStoreBytes: 1 << 20, MaxItemBytes: 64}),
		APIConfig{},
	)
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	// Seed a small regular item: 48+1+1+1 = 51 ≤ 64.
	if code, _, raw := doRequest(t, mux, "POST", "/append",
		`{"scope":"t","id":"y","payload":5}`); code != 200 {
		t.Fatalf("seed append: code=%d body=%s", code, raw)
	}

	// /counter_add at the same scope/id would promote to a counter
	// shape (48+1+1+56 = 106 > 64). The validator must reject.
	code, _, raw := doRequest(t, mux, "POST", "/counter_add", `{"scope":"t","id":"y","by":1}`)
	if code != http.StatusBadRequest {
		t.Fatalf("promote-path /counter_add: code=%d want 400 (body=%s)", code, raw)
	}

	// And the original `5` must still be there.
	_, out, _ := doRequest(t, mux, "GET", "/get?scope=t&id=y", "")
	if !mustBool(t, out, "hit") {
		t.Fatal("seed item lost after rejected promote")
	}
	item := out["item"].(map[string]interface{})
	if v, _ := item["payload"].(float64); v != 5 {
		t.Errorf("rejected promote mutated payload: %v want 5", item["payload"])
	}
}

func TestCounterAdd_BadRequestOnMissingScope(t *testing.T) {
	h, _ := newTestHandler(10)
	code, _, _ := doRequest(t, h, "POST", "/counter_add", `{"id":"k","by":1}`)
	if code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400", code)
	}
}

func TestCounterAdd_BadRequestOnByOutOfRange(t *testing.T) {
	h, _ := newTestHandler(10)
	// 2^53 is one past MaxCounterValue.
	code, _, _ := doRequest(t, h, "POST", "/counter_add", `{"scope":"c","id":"k","by":9007199254740992}`)
	if code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400", code)
	}
}

func TestCounterAdd_BadRequestOnOverflow(t *testing.T) {
	h, _ := newTestHandler(10)
	// Seed at the maximum allowed counter value.
	_, _, _ = doRequest(t, h, "POST", "/counter_add", `{"scope":"c","id":"k","by":9007199254740991}`)

	code, _, _ := doRequest(t, h, "POST", "/counter_add", `{"scope":"c","id":"k","by":1}`)
	if code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400", code)
	}
}

func TestCounterAdd_MethodNotAllowed(t *testing.T) {
	h, _ := newTestHandler(10)
	code, _, _ := doRequest(t, h, "GET", "/counter_add", "")
	if code != http.StatusMethodNotAllowed {
		t.Fatalf("code=%d want 405", code)
	}
}

// --- /delete ------------------------------------------------------------------

func TestDelete_Hit(t *testing.T) {
	h, _ := newTestHandler(10)
	_, _, _ = doRequest(t, h, "POST", "/append", `{"scope":"s","id":"a","payload":{"v":1}}`)

	code, out, _ := doRequest(t, h, "POST", "/delete", `{"scope":"s","id":"a"}`)
	if code != 200 {
		t.Fatalf("code=%d want 200", code)
	}
	if !mustBool(t, out, "hit") {
		t.Error("hit=false")
	}
}

func TestDelete_Miss(t *testing.T) {
	h, _ := newTestHandler(10)
	code, out, _ := doRequest(t, h, "POST", "/delete", `{"scope":"s","id":"none"}`)
	if code != 200 {
		t.Fatalf("code=%d want 200", code)
	}
	if mustBool(t, out, "hit") {
		t.Error("hit=true for missing id")
	}
}

func TestDelete_BySeq_Hit(t *testing.T) {
	h, _ := newTestHandler(10)
	_, _, _ = doRequest(t, h, "POST", "/append", `{"scope":"s","payload":{"v":1}}`)
	_, _, _ = doRequest(t, h, "POST", "/append", `{"scope":"s","payload":{"v":2}}`)

	code, out, _ := doRequest(t, h, "POST", "/delete", `{"scope":"s","seq":1}`)
	if code != 200 {
		t.Fatalf("code=%d want 200", code)
	}
	if !mustBool(t, out, "hit") {
		t.Error("hit=false")
	}
}

func TestDelete_BySeq_Miss(t *testing.T) {
	h, _ := newTestHandler(10)
	code, out, _ := doRequest(t, h, "POST", "/delete", `{"scope":"s","seq":42}`)
	if code != 200 {
		t.Fatalf("code=%d want 200", code)
	}
	if mustBool(t, out, "hit") {
		t.Error("hit=true for missing seq")
	}
}

func TestDelete_RejectsBothIDAndSeq(t *testing.T) {
	h, _ := newTestHandler(10)
	code, _, _ := doRequest(t, h, "POST", "/delete", `{"scope":"s","id":"a","seq":1}`)
	if code != 400 {
		t.Fatalf("code=%d want 400", code)
	}
}

func TestDelete_RejectsNeitherIDNorSeq(t *testing.T) {
	h, _ := newTestHandler(10)
	code, _, _ := doRequest(t, h, "POST", "/delete", `{"scope":"s"}`)
	if code != 400 {
		t.Fatalf("code=%d want 400", code)
	}
}

// --- /delete_scope ------------------------------------------------------------

func TestDeleteScope_Hit(t *testing.T) {
	h, _ := newTestHandler(10)
	_, _, _ = doRequest(t, h, "POST", "/append", `{"scope":"s","id":"a","payload":{"v":1}}`)
	_, _, _ = doRequest(t, h, "POST", "/append", `{"scope":"s","id":"b","payload":{"v":2}}`)

	code, out, _ := doAdminRequest(t, h, "/delete_scope", `{"scope":"s"}`)
	if code != 200 {
		t.Fatalf("code=%d want 200", code)
	}
	if !mustBool(t, out, "hit") {
		t.Error("hit=false")
	}
	if mustFloat(t, out, "count") != 2 {
		t.Errorf("count=%v want 2", out["count"])
	}
}

func TestDeleteScope_Miss(t *testing.T) {
	h, _ := newTestHandler(10)
	code, out, _ := doAdminRequest(t, h, "/delete_scope", `{"scope":"nope"}`)
	if code != 200 {
		t.Fatalf("code=%d want 200", code)
	}
	if mustBool(t, out, "hit") {
		t.Error("hit=true for missing scope")
	}
}

// --- /wipe --------------------------------------------------------------------

// /wipe accepts a POST with no body. A non-empty body is simply ignored;
// it has no effect on the operation.
func TestWipe_IgnoresBody(t *testing.T) {
	h, _ := newTestHandler(10)
	_, _, _ = doRequest(t, h, "POST", "/append", `{"scope":"s","id":"x","payload":{"v":1}}`)

	code, out, _ := doAdminRequest(t, h, "/wipe", `{"anything":"goes"}`)
	if code != 200 {
		t.Fatalf("code=%d want 200", code)
	}
	if mustFloat(t, out, "items") != 1 {
		t.Errorf("items=%v want 1", out["items"])
	}
}

// After /wipe fresh writes must succeed — the cap budget is fully released,
// no stale scope state blocks a re-used id, seq counters restart from 1.
func TestWipe_FreshWritesAfterwards(t *testing.T) {
	h, _ := newTestHandler(10)
	for i := 0; i < 3; i++ {
		_, _, _ = doRequest(t, h, "POST", "/append", `{"scope":"s","id":"`+fmt.Sprint(i)+`","payload":{"v":1}}`)
	}

	_, _, _ = doAdminRequest(t, h, "/wipe", "")

	// Re-use an id that existed before the wipe — must succeed.
	code, out, _ := doRequest(t, h, "POST", "/append", `{"scope":"s","id":"0","payload":{"v":42}}`)
	if code != 200 {
		t.Fatalf("re-append after wipe: code=%d want 200", code)
	}
	item := out["item"].(map[string]interface{})
	// seq restarts from 1 because the scope was fully removed.
	if item["seq"].(float64) != 1 {
		t.Errorf("post-wipe seq=%v want 1", item["seq"])
	}
}

// --- /head / /tail ------------------------------------------------------------

func TestHead_DefaultLimitAndMiss(t *testing.T) {
	h, _ := newTestHandler(10)

	// Missing scope param
	code, _, _ := doRequest(t, h, "GET", "/head", "")
	if code != 400 {
		t.Fatalf("code=%d want 400", code)
	}

	// Scope does not exist → 200 hit=false
	code, out, _ := doRequest(t, h, "GET", "/head?scope=none", "")
	if code != 200 {
		t.Fatalf("code=%d want 200", code)
	}
	if mustBool(t, out, "hit") {
		t.Error("hit=true for missing scope")
	}
}

func TestHead_RejectsOffset(t *testing.T) {
	h, _ := newTestHandler(10)
	// offset was dropped on /head — any attempt to use it must 400 so
	// clients are nudged toward after_seq or /tail instead of silently
	// getting position-paged results that drift under /delete_up_to.
	code, _, _ := doRequest(t, h, "GET", "/head?scope=s&offset=1", "")
	if code != 400 {
		t.Fatalf("code=%d want 400", code)
	}
}

func TestHead_DefaultReturnsOldest(t *testing.T) {
	h, _ := newTestHandler(10)
	for i := 0; i < 5; i++ {
		_, _, _ = doRequest(t, h, "POST", "/append", `{"scope":"s","payload":{"v":1}}`)
	}

	// No after_seq → treat as 0 → full scope (oldest first).
	_, out, _ := doRequest(t, h, "GET", "/head?scope=s&limit=3", "")
	items := out["items"].([]interface{})
	if len(items) != 3 {
		t.Fatalf("items=%d want 3", len(items))
	}
	first := items[0].(map[string]interface{})
	if first["seq"].(float64) != 1 {
		t.Errorf("first.seq=%v want 1", first["seq"])
	}
}

func TestHead_WithAfterSeq(t *testing.T) {
	h, _ := newTestHandler(10)
	for i := 0; i < 5; i++ {
		_, _, _ = doRequest(t, h, "POST", "/append", `{"scope":"s","payload":{"v":1}}`)
	}

	// after_seq=2 → only seq 3, 4, 5; limit=2 clips to seq 3, 4.
	_, out, _ := doRequest(t, h, "GET", "/head?scope=s&limit=2&after_seq=2", "")
	if mustFloat(t, out, "count") != 2 {
		t.Fatalf("count=%v want 2", out["count"])
	}
	items := out["items"].([]interface{})
	first := items[0].(map[string]interface{})
	if first["seq"].(float64) != 3 {
		t.Errorf("first.seq=%v want 3", first["seq"])
	}
}

func TestHead_RejectsMalformedAfterSeq(t *testing.T) {
	h, _ := newTestHandler(10)
	code, _, _ := doRequest(t, h, "GET", "/head?scope=s&after_seq=notanumber", "")
	if code != 400 {
		t.Fatalf("code=%d want 400", code)
	}
}

func TestTail_WithOffset(t *testing.T) {
	h, _ := newTestHandler(10)
	for i := 0; i < 5; i++ {
		_, _, _ = doRequest(t, h, "POST", "/append", `{"scope":"s","payload":{"v":1}}`)
	}

	_, out, _ := doRequest(t, h, "GET", "/tail?scope=s&limit=2&offset=1", "")
	items := out["items"].([]interface{})
	if len(items) != 2 {
		t.Fatalf("items=%d want 2", len(items))
	}
	last := items[1].(map[string]interface{})
	if last["seq"].(float64) != 4 {
		t.Errorf("last.seq=%v want 4", last["seq"])
	}
}

// --- /get ---------------------------------------------------------------------

func TestGet_RequiresExactlyOneOfIDOrSeq(t *testing.T) {
	h, _ := newTestHandler(10)

	// Neither
	code, _, _ := doRequest(t, h, "GET", "/get?scope=s", "")
	if code != 400 {
		t.Fatalf("neither: code=%d want 400", code)
	}

	// Both
	code, _, _ = doRequest(t, h, "GET", "/get?scope=s&id=a&seq=1", "")
	if code != 400 {
		t.Fatalf("both: code=%d want 400", code)
	}
}

func TestGet_ByIDAndBySeq(t *testing.T) {
	h, _ := newTestHandler(10)
	_, _, _ = doRequest(t, h, "POST", "/append", `{"scope":"s","id":"a","payload":{"v":1}}`)

	_, out, _ := doRequest(t, h, "GET", "/get?scope=s&id=a", "")
	if !mustBool(t, out, "hit") {
		t.Error("by id: hit=false")
	}

	_, out, _ = doRequest(t, h, "GET", "/get?scope=s&seq=1", "")
	if !mustBool(t, out, "hit") {
		t.Error("by seq: hit=false")
	}

	_, out, _ = doRequest(t, h, "GET", "/get?scope=s&id=missing", "")
	if mustBool(t, out, "hit") {
		t.Error("missing id: hit=true")
	}
}

// seq=0 is a malformed cursor (cache-assigned seqs start at 1). Reject
// loudly with 400 rather than silently miss — matches /update and
// /delete which reject seq=0 via validateIDOrSeq.
func TestGet_RejectsSeqZero(t *testing.T) {
	h, _ := newTestHandler(10)
	code, _, raw := doRequest(t, h, "GET", "/get?scope=s&seq=0", "")
	if code != 400 {
		t.Fatalf("/get?seq=0: code=%d body=%s want 400", code, raw)
	}
}

func TestRender_RejectsSeqZero(t *testing.T) {
	h, _ := newTestHandler(10)
	rr := doRawRequest(t, h, "GET", "/render?scope=s&seq=0")
	if rr.Code != 400 {
		t.Fatalf("/render?seq=0: code=%d body=%s want 400", rr.Code, rr.Body.String())
	}
}

// --- /render ------------------------------------------------------------------

// doRawRequest is a slimmed-down variant of doRequest that returns the full
// ResponseRecorder. /render tests need header access (Content-Type) and raw
// body access (no JSON unmarshaling) — doRequest hides both.
func doRawRequest(t *testing.T, h http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestRender_MethodNotAllowed(t *testing.T) {
	h, _ := newTestHandler(10)
	rec := doRawRequest(t, h, "POST", "/render?scope=s&id=a")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("code=%d want 405", rec.Code)
	}
}

func TestRender_RejectsMissingScope(t *testing.T) {
	h, _ := newTestHandler(10)
	rec := doRawRequest(t, h, "GET", "/render?id=a")
	if rec.Code != 400 {
		t.Fatalf("code=%d want 400", rec.Code)
	}
}

func TestRender_RequiresExactlyOneOfIDOrSeq(t *testing.T) {
	h, _ := newTestHandler(10)

	rec := doRawRequest(t, h, "GET", "/render?scope=s")
	if rec.Code != 400 {
		t.Fatalf("neither: code=%d want 400", rec.Code)
	}

	rec = doRawRequest(t, h, "GET", "/render?scope=s&id=a&seq=1")
	if rec.Code != 400 {
		t.Fatalf("both: code=%d want 400", rec.Code)
	}
}

func TestRender_RejectsMalformedSeq(t *testing.T) {
	h, _ := newTestHandler(10)
	rec := doRawRequest(t, h, "GET", "/render?scope=s&seq=notanumber")
	if rec.Code != 400 {
		t.Fatalf("code=%d want 400", rec.Code)
	}
}

// Miss (scope doesn't exist, or scope exists but item doesn't) must return
// 404 with an empty body and a neutral Content-Type — the envelope-free
// contract that distinguishes /render from /get.
func TestRender_MissReturns404EmptyBody(t *testing.T) {
	h, _ := newTestHandler(10)

	rec := doRawRequest(t, h, "GET", "/render?scope=nope&id=a")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing scope: code=%d want 404", rec.Code)
	}
	if body := rec.Body.String(); body != "" {
		t.Errorf("missing scope: body=%q want empty", body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("missing scope: Content-Type=%q want application/octet-stream", ct)
	}

	_, _, _ = doRequest(t, h, "POST", "/append", `{"scope":"s","id":"a","payload":{"v":1}}`)

	rec = doRawRequest(t, h, "GET", "/render?scope=s&id=nonexistent")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing id: code=%d want 404", rec.Code)
	}
	if body := rec.Body.String(); body != "" {
		t.Errorf("missing id: body=%q want empty", body)
	}

	rec = doRawRequest(t, h, "GET", "/render?scope=s&seq=42")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing seq: code=%d want 404", rec.Code)
	}
}

// JSON object payloads are written raw — no envelope, no transformation.
// The consumer gets exactly the bytes that were stored.
func TestRender_JSONObjectPayload(t *testing.T) {
	h, _ := newTestHandler(10)
	_, _, _ = doRequest(t, h, "POST", "/append", `{"scope":"s","id":"a","payload":{"text":"hello"}}`)

	rec := doRawRequest(t, h, "GET", "/render?scope=s&id=a")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("Content-Type=%q want application/octet-stream", ct)
	}
	if body := rec.Body.String(); body != `{"text":"hello"}` {
		t.Errorf("body=%q want %q (raw JSON object, no envelope)", body, `{"text":"hello"}`)
	}
}

func TestRender_JSONArrayPayload(t *testing.T) {
	h, _ := newTestHandler(10)
	_, _, _ = doRequest(t, h, "POST", "/append", `{"scope":"s","id":"a","payload":[1,2,3]}`)

	rec := doRawRequest(t, h, "GET", "/render?scope=s&id=a")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200", rec.Code)
	}
	if body := rec.Body.String(); body != `[1,2,3]` {
		t.Errorf("body=%q want raw JSON array", body)
	}
}

// The core use case: HTML/XML/text stored as a JSON string. /render must
// strip exactly one layer of JSON string-encoding so the consumer receives
// real HTML bytes, not the quoted/escaped form. Without this, a browser
// served by Caddy would receive a literal `"<html>..."` and render it as
// text instead of as a webpage.
func TestRender_JSONStringPayload_DecodesOneLayer(t *testing.T) {
	h, _ := newTestHandler(10)
	htmlBody := "<html><body>Hi \"quoted\" and\nnewline and \\ backslash</body></html>"
	encodedPayload, err := json.Marshal(htmlBody)
	if err != nil {
		t.Fatalf("marshal htmlBody: %v", err)
	}
	appendBody := fmt.Sprintf(`{"scope":"pages","id":"home","payload":%s}`, encodedPayload)
	if code, _, raw := doRequest(t, h, "POST", "/append", appendBody); code != 200 {
		t.Fatalf("append: code=%d body=%s", code, raw)
	}

	rec := doRawRequest(t, h, "GET", "/render?scope=pages&id=home")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200", rec.Code)
	}
	if got := rec.Body.String(); got != htmlBody {
		t.Errorf("body=%q want %q (JSON string must be decoded one layer)", got, htmlBody)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("Content-Type=%q want application/octet-stream (cache does not sniff MIME)", ct)
	}
}

func TestRender_BySeq(t *testing.T) {
	h, _ := newTestHandler(10)
	_, _, _ = doRequest(t, h, "POST", "/append", `{"scope":"s","payload":{"v":7}}`)

	rec := doRawRequest(t, h, "GET", "/render?scope=s&seq=1")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200", rec.Code)
	}
	if body := rec.Body.String(); body != `{"v":7}` {
		t.Errorf("body=%q want raw JSON object", body)
	}
}

// /render hits bump scope read-bookkeeping the same way /get hits do.
// Misses must not count (same rule as /get) — otherwise a hot 404
// would skew downstream observability.
func TestRender_HitBumpsReadCount_MissDoesNot(t *testing.T) {
	h, api := newTestHandler(10)
	_, _, _ = doRequest(t, h, "POST", "/append", `{"scope":"s","id":"a","payload":{"v":1}}`)

	_ = doRawRequest(t, h, "GET", "/render?scope=s&id=a")
	_ = doRawRequest(t, h, "GET", "/render?scope=s&id=a")
	_ = doRawRequest(t, h, "GET", "/render?scope=s&id=nonexistent") // miss — must not count

	buf, _ := api.store.getScope("s")
	got := buf.readCountTotal.Load()
	if got != 2 {
		t.Errorf("read_count_total=%d want 2 (two hits, miss must not count)", got)
	}
}

// --- /scopelist ---------------------------------------------------------------

// mustScopelistEntries pulls the typed list of {scope, item_count, ...}
// rows out of a /scopelist response. Lets tests read the wire shape
// without re-asserting the same json.RawMessage dance per case.
func mustScopelistEntries(t *testing.T, out map[string]interface{}) []map[string]interface{} {
	t.Helper()
	raw, ok := out["scopes"]
	if !ok {
		t.Fatalf("missing 'scopes' in /scopelist response: %+v", out)
	}
	arr, ok := raw.([]interface{})
	if !ok {
		t.Fatalf("'scopes' is not an array: %T", raw)
	}
	out2 := make([]map[string]interface{}, 0, len(arr))
	for i, e := range arr {
		m, ok := e.(map[string]interface{})
		if !ok {
			t.Fatalf("scopes[%d] is not an object: %T", i, e)
		}
		out2 = append(out2, m)
	}
	return out2
}

func mustScopeNames(t *testing.T, out map[string]interface{}) []string {
	t.Helper()
	entries := mustScopelistEntries(t, out)
	names := make([]string, 0, len(entries))
	for i, e := range entries {
		s, ok := e["scope"].(string)
		if !ok {
			t.Fatalf("scopes[%d].scope is not a string: %v", i, e["scope"])
		}
		names = append(names, s)
	}
	return names
}

// Cursor pagination: limit + after is the only paging mode shipped.
// `truncated` flips when more matching scopes exist past the page,
// and resuming with after=<last scope> walks the next page.
//
// Reserved scopes (_events, _inbox) sort before every user scope because
// '_' (0x5F) < 'a' (0x61). This test uses after=_zzz as the initial
// cursor to skip past reserved scopes and exercise pagination on the
// user-managed scopes alone.
func TestScopelist_LimitAndAfterCursor(t *testing.T) {
	h, _ := newTestHandler(10)
	scopes := []string{"a", "b", "c", "d", "e"}
	for _, s := range scopes {
		body := fmt.Sprintf(`{"scope":%q,"payload":{"v":1}}`, s)
		_, _, _ = doRequest(t, h, "POST", "/append", body)
	}

	_, out, _ := doRequest(t, h, "GET", "/scopelist?limit=2&after=_zzz", "")
	if !mustBool(t, out, "truncated") {
		t.Error("truncated=false on limit=2 with 5 scopes")
	}
	got := mustScopeNames(t, out)
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("page1=%v want [a b]", got)
	}

	_, out, _ = doRequest(t, h, "GET", "/scopelist?limit=2&after=b", "")
	if !mustBool(t, out, "truncated") {
		t.Error("truncated=false on second page with more behind")
	}
	got = mustScopeNames(t, out)
	if len(got) != 2 || got[0] != "c" || got[1] != "d" {
		t.Errorf("page2=%v want [c d]", got)
	}

	_, out, _ = doRequest(t, h, "GET", "/scopelist?limit=2&after=d", "")
	if mustBool(t, out, "truncated") {
		t.Error("truncated=true on final page")
	}
	got = mustScopeNames(t, out)
	if len(got) != 1 || got[0] != "e" {
		t.Errorf("page3=%v want [e]", got)
	}

	// after past every scope name → empty page, not truncated.
	_, out, _ = doRequest(t, h, "GET", "/scopelist?after=zzz", "")
	if mustFloat(t, out, "count") != 0 {
		t.Errorf("after=zzz: count=%v want 0", out["count"])
	}
}

// /scopelist must not bump per-scope read-bookkeeping. Eviction-candidate
// addons that poll /scopelist would otherwise see their own polls inflate
// the read_count_total they're trying to measure. Same rule the RFC §8.2
// applies to /stats.
func TestScopelist_DoesNotBumpReadBookkeeping(t *testing.T) {
	h, api := newTestHandler(10)
	_, _, _ = doRequest(t, h, "POST", "/append", `{"scope":"x","id":"a","payload":{"v":1}}`)

	for i := 0; i < 5; i++ {
		_, _, _ = doRequest(t, h, "GET", "/scopelist", "")
	}

	buf, ok := api.store.getScope("x")
	if !ok {
		t.Fatal("scope x missing")
	}
	if got := buf.readCountTotal.Load(); got != 0 {
		t.Errorf("read_count_total=%d want 0 (/scopelist must not count as a read)", got)
	}
	if got := buf.lastAccessTS.Load(); got != 0 {
		t.Errorf("last_access_ts=%d want 0 (/scopelist must not stamp access)", got)
	}
}

// Validation errors share the standard 400 envelope: prefix and after both
// flow through checkKeyField (same shape rules as scope), so an embedded
// control character or oversize value is rejected up-front.
func TestScopelist_ValidationErrors(t *testing.T) {
	h, _ := newTestHandler(10)
	cases := []struct{ url, wantSubstr string }{
		{"/scopelist?prefix=" + strings.Repeat("a", MaxScopeBytes+1), "256"},
		{"/scopelist?after=" + strings.Repeat("b", MaxScopeBytes+1), "256"},
		{"/scopelist?limit=0", "positive"},
		{"/scopelist?limit=-3", "positive"},
		{"/scopelist?limit=abc", "positive"},
	}
	for _, c := range cases {
		code, out, raw := doRequest(t, h, "GET", c.url, "")
		if code != 400 {
			t.Errorf("%s: code=%d want 400 body=%s", c.url, code, raw)
			continue
		}
		errStr, _ := out["error"].(string)
		if !strings.Contains(errStr, c.wantSubstr) {
			t.Errorf("%s: error=%q does not mention %q", c.url, errStr, c.wantSubstr)
		}
	}
}

func TestScopelist_MethodNotAllowed(t *testing.T) {
	h, _ := newTestHandler(10)
	code, _, _ := doRequest(t, h, "POST", "/scopelist", "")
	if code != 405 {
		t.Errorf("POST /scopelist: code=%d want 405", code)
	}
}

// Empty store → empty array, count=0, truncated=false. Wire-format check
// that no client sees null instead of []. Uses after=_zzz to skip past
// the reserved scopes (_events, _inbox) that NewStore pre-creates so the
// "empty" assertion exercises the empty-result code path.
func TestScopelist_EmptyStore(t *testing.T) {
	h, _ := newTestHandler(10)
	_, out, _ := doRequest(t, h, "GET", "/scopelist?after=_zzz", "")
	if mustFloat(t, out, "count") != 0 {
		t.Errorf("count=%v want 0", out["count"])
	}
	if mustBool(t, out, "truncated") {
		t.Error("truncated=true on empty store")
	}
	entries := mustScopelistEntries(t, out)
	if len(entries) != 0 {
		t.Errorf("scopes=%v want []", entries)
	}
}

// Sanity: the seven primitives carry the values the buffer actually holds.
// Reading a per-scope row from /scopelist must report the same numbers as
// poking the *scopeBuffer directly.
func TestScopelist_ReportsPerScopePrimitives(t *testing.T) {
	h, api := newTestHandler(10)
	_, _, _ = doRequest(t, h, "POST", "/append", `{"scope":"x","id":"a","payload":{"v":1}}`)
	_, _, _ = doRequest(t, h, "POST", "/append", `{"scope":"x","id":"b","payload":{"v":2}}`)
	_, _, _ = doRequest(t, h, "GET", "/get?scope=x&id=a", "") // bumps read_count_total + last_access_ts

	_, out, _ := doRequest(t, h, "GET", "/scopelist?prefix=x", "")
	row := mustScopelistEntries(t, out)[0]
	if row["scope"].(string) != "x" {
		t.Errorf("scope=%v want x", row["scope"])
	}
	if row["item_count"].(float64) != 2 {
		t.Errorf("item_count=%v want 2", row["item_count"])
	}
	if row["last_seq"].(float64) != 2 {
		t.Errorf("last_seq=%v want 2", row["last_seq"])
	}
	if row["read_count_total"].(float64) != 1 {
		t.Errorf("read_count_total=%v want 1 (one /get hit)", row["read_count_total"])
	}

	buf, _ := api.store.getScope("x")
	wantLastWrite := buf.lastWriteTS
	if got := int64(row["last_write_ts"].(float64)); got != wantLastWrite {
		t.Errorf("last_write_ts=%d want %d", got, wantLastWrite)
	}
}

// --- integration: mixed workload ---------------------------------------------

// --- integration: parallel race workload -------------------------------------

// TestRace_ParallelMixedWorkload hammers the API from many goroutines at once
// and checks the state that survives against concretely tallied expectations.
//
// Each worker keeps its own counters (successful appends, deleted items via
// /delete and /delete_up_to, reads-with-hit) so the hot loop is lock-free. At
// the end we sum the tallies and require the API's own state to match to the
// item. Everything we can derive from the workload is checked exactly:
//
//   - items from /stats == Σ appendsOK − Σ deletedN
//   - same total matches len(items) walked across every live scope
//   - per scope: items slice, bySeq map, byID map are mutually consistent
//   - per scope: items are still sorted ascending by seq (append contract)
//   - per scope: lastSeq >= the highest seq present (never rewinds)
//   - store.totalBytes == Σ buf.bytes == Σ approxItemSize recomputed from items
//   - Σ buf.readCountTotal == Σ readsHit (the workers only issue reads that
//     trigger recordRead, so the two counts are equal, not just related)
//
// Ops per workload are chosen to exercise every mutation path that takes the
// scope write lock (append, delete, update, delete_up_to) as well as the read
// paths that take RLock + recordRead. /warm, /rebuild and /delete_scope are
// intentionally excluded here: they wipe or swap scope state, which would
// destroy the "Σ appends − Σ deletes = current items" relation that makes the
// concrete check possible. Races touching those paths live in separate tests.
func TestRace_ParallelMixedWorkload(t *testing.T) {
	const (
		workers      = 32
		opsPerWorker = 2000
		scopeCap     = 20000 // head-room: ~3.6k avg appends/scope with skew slack
	)
	scopes := []string{"sa", "sb", "sc", "sd", "se", "sf", "sg", "sh"}

	h, api := newTestHandler(scopeCap)

	type tally struct {
		appendsOK int64 // successful /append (200 response)
		deletedN  int64 // sum of count from /delete and /delete_up_to
		readsHit  int64 // /head and /tail calls that returned hit=true
	}
	tallies := make([]tally, workers)

	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func(id int) {
			defer wg.Done()
			// Each worker gets its own RNG so the hot path has zero shared
			// state. Seeds are worker-unique so different workers explore
			// different op/seq sequences.
			rng := rand.New(rand.NewSource(int64(id) + 1))
			ts := &tallies[id]

			for i := 0; i < opsPerWorker; i++ {
				scope := scopes[rng.Intn(len(scopes))]
				switch roll := rng.Intn(100); {
				case roll < 45:
					// Append without id. Skipping id avoids dup-id rejections
					// muddying the appendsOK tally — every 200 really is a new item.
					body := fmt.Sprintf(`{"scope":"%s","payload":{"w":%d,"i":%d}}`, scope, id, i)
					if code, _, _ := doRequest(t, h, "POST", "/append", body); code == 200 {
						ts.appendsOK++
					}
				case roll < 65:
					// Delete by seq. Random seq in [1, 500] — most miss, which
					// is fine: we only count actual hits via count.
					seq := rng.Intn(500) + 1
					body := fmt.Sprintf(`{"scope":"%s","seq":%d}`, scope, seq)
					if _, out, _ := doRequest(t, h, "POST", "/delete", body); out != nil {
						if n, ok := out["count"].(float64); ok {
							ts.deletedN += int64(n)
						}
					}
				case roll < 75:
					// Update by seq — doesn't change item count but exercises
					// the byte-delta reservation path under the write lock.
					seq := rng.Intn(500) + 1
					body := fmt.Sprintf(`{"scope":"%s","seq":%d,"payload":{"u":%d}}`, scope, seq, i)
					_, _, _ = doRequest(t, h, "POST", "/update", body)
				case roll < 85:
					path := fmt.Sprintf("/head?scope=%s&limit=5", scope)
					if _, out, _ := doRequest(t, h, "GET", path, ""); out != nil {
						if hit, ok := out["hit"].(bool); ok && hit {
							ts.readsHit++
						}
					}
				case roll < 95:
					path := fmt.Sprintf("/tail?scope=%s&limit=5", scope)
					if _, out, _ := doRequest(t, h, "GET", path, ""); out != nil {
						if hit, ok := out["hit"].(bool); ok && hit {
							ts.readsHit++
						}
					}
				default:
					// Delete_up_to with a small max_seq. Targets the oldest
					// slice of each scope so the prefix drain path is hammered
					// while appends are still extending the tail.
					maxSeq := rng.Intn(50) + 1
					body := fmt.Sprintf(`{"scope":"%s","max_seq":%d}`, scope, maxSeq)
					if _, out, _ := doRequest(t, h, "POST", "/delete_up_to", body); out != nil {
						if n, ok := out["count"].(float64); ok {
							ts.deletedN += int64(n)
						}
					}
				}
			}
		}(w)
	}
	wg.Wait()

	var appendsOK, deletedN, readsHit int64
	for _, s := range tallies {
		appendsOK += s.appendsOK
		deletedN += s.deletedN
		readsHit += s.readsHit
	}
	t.Logf("race workload: appendsOK=%d deletedN=%d readsHit=%d (workers=%d ops/worker=%d)",
		appendsOK, deletedN, readsHit, workers, opsPerWorker)

	expectedItems := appendsOK - deletedN
	if expectedItems < 0 {
		t.Fatalf("tally arithmetic impossible: appendsOK=%d < deletedN=%d", appendsOK, deletedN)
	}

	_, stats, _ := doAdminRequest(t, h, "/stats", "")
	if got := int64(mustFloat(t, stats, "items")); got != expectedItems {
		t.Errorf("/stats items=%d want %d (appends=%d deletes=%d)",
			got, expectedItems, appendsOK, deletedN)
	}

	// Walk the store directly and re-verify every invariant.
	var sumBufBytes, recomputedBytes int64
	var totalItemsWalked int64
	var totalReadCount uint64
	storeScopes := api.store.listScopes()
	scopeOverhead := int64(len(storeScopes)) * scopeBufferOverhead
	for scopeName, buf := range storeScopes {
		buf.mu.RLock()
		sumBufBytes += buf.bytes
		totalItemsWalked += int64(len(buf.items))
		totalReadCount += buf.readCountTotal.Load()

		if got, want := len(buf.bySeq), len(buf.items); got != want {
			t.Errorf("scope %q: len(bySeq)=%d != len(items)=%d", scopeName, got, want)
		}
		nonEmptyIDs := 0
		for i := range buf.items {
			if buf.items[i].ID != "" {
				nonEmptyIDs++
			}
		}
		if got := len(buf.byID); got != nonEmptyIDs {
			t.Errorf("scope %q: len(byID)=%d != items-with-non-empty-id=%d", scopeName, got, nonEmptyIDs)
		}
		for i := 1; i < len(buf.items); i++ {
			if buf.items[i].Seq <= buf.items[i-1].Seq {
				t.Errorf("scope %q: items out of order at index %d: seq=%d <= prev=%d",
					scopeName, i, buf.items[i].Seq, buf.items[i-1].Seq)
				break
			}
		}
		if n := len(buf.items); n > 0 {
			if maxSeq := buf.items[n-1].Seq; buf.lastSeq < maxSeq {
				t.Errorf("scope %q: lastSeq=%d < max item seq=%d (must never rewind)",
					scopeName, buf.lastSeq, maxSeq)
			}
		}
		for i := range buf.items {
			recomputedBytes += approxItemSize(*buf.items[i])
		}
		buf.mu.RUnlock()
	}

	if totalItemsWalked != expectedItems {
		t.Errorf("items-walked-from-store=%d != expectedItems=%d", totalItemsWalked, expectedItems)
	}
	if got := api.store.totalBytes.Load(); got != sumBufBytes+scopeOverhead {
		t.Errorf("totalBytes=%d != Σ buf.bytes + overhead=%d (ghost bytes)", got, sumBufBytes+scopeOverhead)
	}
	if got := api.store.totalBytes.Load(); got != recomputedBytes+scopeOverhead {
		t.Errorf("totalBytes=%d != recomputed-from-items + overhead=%d (counter drift)", got, recomputedBytes+scopeOverhead)
	}
	// /head and /tail are the only read paths the workers use, and both call
	// recordRead exactly once on a hit. So Σ readCountTotal must equal the
	// sum of the workers' readsHit tallies — no approximation.
	if int64(totalReadCount) != readsHit {
		t.Errorf("Σ readCountTotal=%d != tallied readsHit=%d", totalReadCount, readsHit)
	}

	// Stats counters must agree with the post-race ground truth — same
	// invariant as in the single-threaded mixed-workload test, but here
	// it also exercises every concurrent write/delete path against the
	// atomic counters.
	assertStatsCountersInvariant(t, api.store, "after parallel race workload")
}

// --- Reserved-scope HTTP-level integration tests -----------------------------
//
// These exercise the reservation contract end-to-end via the HTTP handler
// rather than via the validator or Store-method layer alone. They pin the
// guarantees an operator actually relies on:
//   - /append on _inbox round-trips the item, and the per-scope counters
//     in /scopelist plus the store-wide counters in /stats reflect the
//     append correctly.
//   - The drainer pattern (append → tail → delete_up_to) actually frees
//     items + bytes on a reserved scope.
//   - The HTTP layer rejects every operation that the reservation contract
//     forbids (/upsert, /update, /counter_add, /delete_scope, /warm,
//     /rebuild) with status 400.
//   - /wipe restores the reserved-scope baseline (scopes=2, items=0,
//     small but non-zero approx_store_mb) so subscribers attached to
//     either reserved scope find their target still present.

// newReservedScopesTestHandler constructs a Store + API with a custom
// Config so the per-reserved-scope cap tests can drive the knobs that
// the default newTestHandler doesn't expose. Same shape as that helper
// — accepts Config, hands you (mux, api).
func newReservedScopesTestHandler(t *testing.T, cfg Config) (http.Handler, *API) {
	t.Helper()
	api := NewAPI(NewGateway(cfg), APIConfig{})
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)
	return mux, api
}

// _events's per-item byte cap derivation (max(MaxItemBytes,
// Inbox.MaxItemBytes) + 1 KiB envelope slack) is verified via:
//
//   - TestNewStore_DerivesReservedScopeCaps — the field-level
//     derivation under several Config shapes.
//   - TestEvents_AutoPopulate_FullModeNoDropOnLargeInboxWrite — the
//     end-to-end path: a large _inbox /append in EventsModeFull
//     auto-populates an event that fits the derived cap.
//
// Direct /append on _events used to drive a third synthetic check
// here ("does _events accept a payload past the user's MaxItemBytes
// but within the derived cap"), but with /append _events now closed
// the synthetic path is gone. The two coverage points above test
// the same underlying derivation through the only paths that can
// still reach it.

// eventsTailCount is a tiny helper for the auto-populate tests that
// follow: hits /tail on `_events`, asserts the response is OK, and
// returns the count + items list. Mirrors the inlined pattern used
// elsewhere in this file but factored out because the auto-populate
// tests all need the same probe.
func eventsTailCount(t *testing.T, h http.Handler) (int, []map[string]interface{}) {
	t.Helper()
	code, out, raw := doRequest(t, h, "GET", "/tail?scope=_events&limit=100", "")
	if code != 200 {
		t.Fatalf("/tail _events: code=%d body=%s", code, raw)
	}
	count := int(mustFloat(t, out, "count"))
	rawItems, _ := out["items"].([]interface{})
	items := make([]map[string]interface{}, 0, len(rawItems))
	for _, ri := range rawItems {
		if m, ok := ri.(map[string]interface{}); ok {
			items = append(items, m)
		}
	}
	return count, items
}

// --- response marshal-failure path --------------------------------------------
//
// The pair of TestWriteJSONWithDuration_MarshalFailureReturns500 +
// TestWriteJSONWithMeta_MarshalFailureReturns500 tests pinned an
// emergent json.Marshal-detected behaviour: corrupt RawMessage bytes
// inside a marshalled envelope produced HTTP 500 because json.Marshal
// returned an error and the helper short-circuited.
//
// That contract was dropped when the read paths were rewritten to
// stream payload bytes directly (handlers_read.go writeGetResponse,
// writeItemsResponse, writeScopeListResponse) — and again when
// writeJSONWithDuration switched to the appendKVValue fast path
// shared with the read writers (handlers.go). All three writers now
// trust the write-time validatePayload guarantee; any post-write
// corruption is the caller's problem, in line with the project
// policy of validating only at system boundaries (CLAUDE.md).
//
// In production this code path is unreachable: write-endpoint
// envelopes never carry an Item-with-Payload (writeAck excludes
// Payload by design, see handlers_write.go), and the read writers
// never go through writeJSONWithDuration at all. The two old tests
// only triggered through direct white-box calls; the helper they
// asserted against has no remaining production caller that can
// reproduce the failure mode.

// /get and /render both serve stored payload bytes as-is at read
// time. Validation happens at write time (validatePayload rejects
// malformed JSON before it reaches the buffer); the read path
// trusts that what is in the buffer was once valid. If memory
// corruption or an internal bug ever produces a buffer with
// malformed bytes, both endpoints will emit those bytes — the
// resulting HTTP response body is invalid JSON, but the HTTP
// layer itself succeeds with 200.
//
// Earlier versions of /get picked up implicit validation as a
// side effect of routing through json.Marshal; that path was
// removed when /get was rewritten to stream payload bytes
// directly to the wire (handlers_read.go writeGetResponse), in
// line with the project policy of validating only at system
// boundaries (CLAUDE.md). This test now locks in the matching
// "serve as-is" contract for /get so the symmetry with /render
// can be enforced.
//
// White-box access via api.store is intentional — there is no
// non-internal way to construct this state (validatePayload
// blocks every write path).
func TestGet_CorruptStoredPayloadServesAsIs(t *testing.T) {
	h, api := newTestHandler(10)

	if code, _, _ := doRequest(t, h, "POST", "/append",
		`{"scope":"s","id":"a","payload":{"v":1}}`); code != 200 {
		t.Fatalf("seed append: code=%d want 200", code)
	}

	sh := api.store.shardFor("s")
	sh.mu.RLock()
	buf := sh.scopes["s"]
	sh.mu.RUnlock()
	if buf == nil {
		t.Fatal("scope buffer for 's' missing after append")
	}

	buf.mu.Lock()
	corrupt := buf.items[0]
	corrupt.Payload = json.RawMessage(`{"a":`)
	corrupt.renderBytes = nil
	buf.items[0] = corrupt
	buf.byID["a"] = corrupt
	buf.bySeq[corrupt.Seq] = corrupt
	buf.mu.Unlock()

	rec := doRawRequest(t, h, "GET", "/get?scope=s&id=a")
	if rec.Code != http.StatusOK {
		t.Fatalf("/get on corrupt payload: code=%d want 200 (read path trusts write-time validation)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"payload":{"a":`) {
		t.Fatalf("/get response should include the (corrupt) payload bytes verbatim, got %q", rec.Body.String())
	}
}

func mapKeys(m map[string]interface{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// /get on a counter item must emit the materialised current value,
// not the stale Payload bytes that live on the surrounding Item.
// The materialisation happens in buffer_read.go before the item
// reaches the handler — this test pins the round-trip so a future
// regression in materialiseCounter (or in the handler bypassing
// it) is caught.
func TestGet_CounterItemReflectsMaterialisedValue(t *testing.T) {
	h, _ := newTestHandler(10)

	if code, _, raw := doRequest(t, h, "POST", "/counter_add",
		`{"scope":"views","id":"page","by":7}`); code != 200 {
		t.Fatalf("seed counter_add: code=%d body=%s want 200", code, raw)
	}
	if code, _, raw := doRequest(t, h, "POST", "/counter_add",
		`{"scope":"views","id":"page","by":3}`); code != 200 {
		t.Fatalf("increment counter_add: code=%d body=%s want 200", code, raw)
	}

	_, out, raw := doRequest(t, h, "GET", "/get?scope=views&id=page", "")
	if !mustBool(t, out, "hit") {
		t.Fatalf("/get counter: hit=false; raw=%q", raw)
	}
	item, ok := out["item"].(map[string]interface{})
	if !ok {
		t.Fatalf("/get counter: item is not an object; raw=%q", raw)
	}
	payload, ok := item["payload"]
	if !ok {
		t.Fatalf("/get counter: response missing payload field; raw=%q", raw)
	}
	got, ok := payload.(float64)
	if !ok {
		t.Fatalf("/get counter: payload is %T %v, expected JSON number; raw=%q", payload, payload, raw)
	}
	if got != 10 {
		t.Errorf("/get counter payload=%v want 10", got)
	}
}

// /get must JSON-escape scope and id strings that contain
// JSON-special bytes (quote, backslash, control chars). The
// fast path uses json.Marshal on these strings to inherit the
// stdlib's escape rules; this test pins the contract.
func TestGet_EscapesJSONSpecialCharactersInScopeAndID(t *testing.T) {
	h, _ := newTestHandler(10)

	// validatePayload rejects control chars in scope/id, so we
	// stick to JSON-special bytes that ARE allowed: quote and
	// backslash. Both still need escaping in the /get response
	// to keep the JSON well-formed.
	scope := `weird"scope\name`
	id := `id"with\backslashes`

	body := `{"scope":` + jsonString(scope) + `,"id":` + jsonString(id) + `,"payload":{"v":1}}`
	if code, _, raw := doRequest(t, h, "POST", "/append", body); code != 200 {
		t.Fatalf("seed /append with special chars: code=%d body=%s want 200", code, raw)
	}

	rec := doRawRequest(t, h, "GET", "/get?scope="+urlQueryEscape(scope)+"&id="+urlQueryEscape(id))
	if rec.Code != http.StatusOK {
		t.Fatalf("/get special chars: code=%d body=%q want 200", rec.Code, rec.Body.String())
	}
	respBody := rec.Body.String()
	if !json.Valid([]byte(strings.TrimRight(respBody, "\n"))) {
		t.Fatalf("/get response is not valid JSON: %q", respBody)
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(respBody), &parsed); err != nil {
		t.Fatalf("/get response unmarshal: %v; body=%q", err, respBody)
	}
	item, ok := parsed["item"].(map[string]interface{})
	if !ok {
		t.Fatalf("/get item is not an object; body=%q", respBody)
	}
	if item["scope"] != scope {
		t.Errorf("scope round-trip: got %q want %q", item["scope"], scope)
	}
	if item["id"] != id {
		t.Errorf("id round-trip: got %q want %q", item["id"], id)
	}
}

// /get with an artificially nil-payload Item (constructable only
// via white-box buffer mutation; validatePayload blocks it on the
// write path) must emit `"payload":null` rather than corrupt
// `"payload":}`. The defensive guard in writeGetResponse restores
// the json.Marshal(RawMessage(nil)) behaviour the old path had.
func TestGet_NilPayloadEmitsLiteralNull(t *testing.T) {
	h, api := newTestHandler(10)

	if code, _, _ := doRequest(t, h, "POST", "/append",
		`{"scope":"s","id":"a","payload":{"v":1}}`); code != 200 {
		t.Fatal("seed append failed")
	}

	sh := api.store.shardFor("s")
	sh.mu.RLock()
	buf := sh.scopes["s"]
	sh.mu.RUnlock()

	buf.mu.Lock()
	mutated := buf.items[0]
	mutated.Payload = nil
	mutated.renderBytes = nil
	buf.items[0] = mutated
	buf.byID["a"] = mutated
	buf.bySeq[mutated.Seq] = mutated
	buf.mu.Unlock()

	rec := doRawRequest(t, h, "GET", "/get?scope=s&id=a")
	if rec.Code != http.StatusOK {
		t.Fatalf("/get nil payload: code=%d body=%q", rec.Code, rec.Body.String())
	}
	if !json.Valid([]byte(strings.TrimRight(rec.Body.String(), "\n"))) {
		t.Fatalf("/get nil payload produced invalid JSON: %q", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"payload":null`) {
		t.Errorf("/get nil payload should emit \"payload\":null; got %q", rec.Body.String())
	}
}

// Helpers used by the special-character round-trip test only —
// the rest of the file uses url.QueryEscape inline where needed.
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func urlQueryEscape(s string) string {
	// net/url.QueryEscape would be cleaner but adding the import
	// just for this helper feels heavy; the test inputs only contain
	// ASCII bytes that need escaping, so we hand-write it.
	var b strings.Builder
	for _, r := range []byte(s) {
		switch r {
		case '"', '\\', '\t', '\n', ' ', '&', '?', '=', '+', '%', '#':
			fmt.Fprintf(&b, "%%%02X", r)
		default:
			b.WriteByte(r)
		}
	}
	return b.String()
}
