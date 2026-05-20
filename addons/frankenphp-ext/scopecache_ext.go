// scopecache_ext.go — FrankenPHP extension exposing scopecache's
// Go-level *Gateway directly to PHP, cutting out the loopback-HTTP
// transport that other PHP clients pay.
//
// PHP and the Caddy module share one *Gateway through the process-
// wide named registry in gateway_registry.go: the caddymodule
// registers under "default" during Provision(); defaultSlot here
// caches the slot pointer at package init and Loads it per call.
// Caddy reload swaps the slot atomically — no invalidation here.
//
// Return-shape contract: every //export_php function below maps 1:1
// to its HTTP-endpoint counterpart in RFC §6 and returns the SAME
// wire bytes the HTTP endpoint would emit — `?string` for every
// function. The two payload-only reads (scopecache_get_payload,
// scopecache_get_payload_by_seq) return raw payload bytes, same as
// GET /render. Every other function returns the full JSON envelope
// as a string, byte-identical to the HTTP response. Consumers that
// want a PHP array call json_decode($result, true). Errors come back
// as `{"ok":false,"error":"msg"}`. A nil return crosses to PHP as
// null and means "no caddymodule loaded" (Provision never ran).
//
// Why JSON-string returns instead of PHP arrays:
//   - One wire format — symmetric with HTTP, one spec, one set of
//     tests, one shape to learn.
//   - ~3× cheaper on the cgo boundary for single reads (single
//     zend_string_init vs N HashTable allocations).
//   - ~10× cheaper on bulk reads (/head, /tail) because the cgo
//     crossing count drops from O(N×fields) to 1.
//   - Drops ~700 lines of phpAssoc* helpers and C trampolines that
//     existed only to build PHP arrays from Go.
//
// Lifetime contract at the cgo boundary:
//   - Read paths take scope/id as zero-copy zendStringView aliases;
//     the Gateway consumes them synchronously and retains no
//     references.
//   - Write paths MUST deep-copy scope/id via zendStringCopy because
//     those strings become permanent map keys inside scopecache; an
//     alias would point at PHP-arena memory freed at request end.
//   - Payload bytes on writes use zendStringBytes (alias) — safe
//     because Gateway.Append's cloneItemPayload copies at the
//     boundary. Response payload bytes are Gateway-cloned reads, so
//     PHP owns them outright after phpStringFromBytes.

package scopecache_ext

/*
#include <php.h>
*/
import "C"

import (
	"fmt"
	"sync/atomic"
	"unsafe"

	"github.com/VeloxCoding/scopecache"
	"github.com/dunglas/frankenphp"
)

// defaultSlot is the atomic *Gateway slot for the "default" name,
// resolved once at package init. LookupGatewaySlot lazily creates
// the slot, so the order of package init vs. caddymodule
// Provision() does not matter — Load returns nil until Provision
// registers a Gateway, and every //export_php below treats that as
// "no caddymodule loaded".
var defaultSlot *atomic.Pointer[scopecache.Gateway] = scopecache.LookupGatewaySlot("default")

// zendStringView returns a zero-copy Go string aliasing a
// zend_string's bytes. Lifetime is the calling PHP_FUNCTION only;
// callers must not retain the returned string past cgo return.
// Used on read paths — see the lifetime contract in the file header.
func zendStringView(s *C.zend_string) string {
	if s == nil {
		return ""
	}
	return unsafe.String((*byte)(unsafe.Pointer(&s.val)), int(s.len))
}

// zendStringBytes is the []byte twin of zendStringView, same lifetime.
func zendStringBytes(s *C.zend_string) []byte {
	if s == nil {
		return nil
	}
	return unsafe.Slice((*byte)(unsafe.Pointer(&s.val)), int(s.len))
}

// zendStringCopy returns a Go-owned copy of a zend_string's bytes.
// Required on write paths — see the lifetime contract in the file header.
func zendStringCopy(s *C.zend_string) string {
	if s == nil {
		return ""
	}
	return C.GoStringN((*C.char)(unsafe.Pointer(&s.val)), C.int(s.len))
}

// phpStringFromBytes emalloc's a fresh zend_string from b and returns
// it as an unsafe.Pointer the //export_php `?string` returns can
// hand to PHP. Empty input returns nil, which the build-time wrapper
// patch (RETURN_EMPTY_STRING→RETURN_NULL) surfaces as PHP null.
func phpStringFromBytes(b []byte) unsafe.Pointer {
	if len(b) == 0 {
		return nil
	}
	return unsafe.Pointer(C.zend_string_init(
		(*C.char)(unsafe.Pointer(&b[0])),
		C.size_t(len(b)),
		C._Bool(false),
	))
}

// errorEnvelope renders an error message as a JSON `{"ok":false,
// "error":"msg"}` envelope and hands it to PHP as a zend_string.
// Used by every write/observability/bulk function on Gateway error.
func errorEnvelope(msg string) unsafe.Pointer {
	body, err := scopecache.MarshalEnvelope(scopecache.ErrorResponse{OK: false, Error: msg})
	if err != nil {
		return phpStringFromBytes([]byte(`{"ok":false,"error":"internal: response marshal failed"}`))
	}
	return phpStringFromBytes(body)
}

// mustMarshal is the write/observability/bulk success-path counterpart
// of errorEnvelope. The wrapped response types in
// scopecache/response_types.go have stable shapes; a Marshal error
// would mean a goccy/go-json runtime issue, in which case we still
// owe PHP a syntactically-valid envelope rather than nil.
func mustMarshal(v any) []byte {
	body, err := scopecache.MarshalEnvelope(v)
	if err != nil {
		return []byte(`{"ok":false,"error":"internal: response marshal failed"}`)
	}
	return body
}

// --- Reads -----------------------------------------------------------

// scopecache_get — GET /get envelope as a JSON string.
//
// export_php:function scopecache_get(string $scope, string $id): ?string
func scopecache_get(scope *C.zend_string, id *C.zend_string) unsafe.Pointer {
	gw := defaultSlot.Load()
	if gw == nil {
		return nil
	}
	item, found := gw.GetByID(zendStringView(scope), zendStringView(id))
	var buf []byte
	if found {
		buf = scopecache.AppendGetResponseJSON(nil, &item)
	} else {
		buf = scopecache.AppendGetResponseJSON(nil, nil)
	}
	return phpStringFromBytes(buf)
}

// scopecache_get_by_seq — GET /get envelope, addressed by seq.
//
// export_php:function scopecache_get_by_seq(string $scope, int $seq): ?string
func scopecache_get_by_seq(scope *C.zend_string, seq int64) unsafe.Pointer {
	gw := defaultSlot.Load()
	if gw == nil {
		return nil
	}
	item, found := gw.GetBySeq(zendStringView(scope), uint64(seq))
	var buf []byte
	if found {
		buf = scopecache.AppendGetResponseJSON(nil, &item)
	} else {
		buf = scopecache.AppendGetResponseJSON(nil, nil)
	}
	return phpStringFromBytes(buf)
}

// scopecache_get_payload — raw payload bytes for (scope,id), no
// envelope. Equivalent to GET /render. JSON-string payloads pass
// through unwrapped; other shapes pass through as canonical JSON.
// Lowest-overhead read path: skip envelope-build entirely.
//
// export_php:function scopecache_get_payload(string $scope, string $id): ?string
func scopecache_get_payload(scope *C.zend_string, id *C.zend_string) unsafe.Pointer {
	gw := defaultSlot.Load()
	if gw == nil {
		return nil
	}
	bytes, found := gw.RenderByID(zendStringView(scope), zendStringView(id))
	if !found {
		return nil
	}
	return phpStringFromBytes(bytes)
}

// scopecache_get_payload_by_seq — payload-only read, addressed by seq.
//
// export_php:function scopecache_get_payload_by_seq(string $scope, int $seq): ?string
func scopecache_get_payload_by_seq(scope *C.zend_string, seq int64) unsafe.Pointer {
	gw := defaultSlot.Load()
	if gw == nil {
		return nil
	}
	bytes, found := gw.RenderBySeq(zendStringView(scope), uint64(seq))
	if !found {
		return nil
	}
	return phpStringFromBytes(bytes)
}

// scopecache_head — GET /head envelope as a JSON string.
//
// export_php:function scopecache_head(string $scope, int $after_seq, int $limit): ?string
func scopecache_head(scope *C.zend_string, afterSeq int64, limit int64) unsafe.Pointer {
	gw := defaultSlot.Load()
	if gw == nil {
		return nil
	}
	items, truncated, _ := gw.Head(zendStringView(scope), uint64(afterSeq), int(limit))
	buf := scopecache.AppendHeadResponseJSON(nil, items, truncated)
	return phpStringFromBytes(buf)
}

// scopecache_tail — GET /tail envelope as a JSON string. `offset` in
// the envelope is always 0; the PHP signature does not expose paging.
//
// export_php:function scopecache_tail(string $scope, int $limit): ?string
func scopecache_tail(scope *C.zend_string, limit int64) unsafe.Pointer {
	gw := defaultSlot.Load()
	if gw == nil {
		return nil
	}
	items, truncated, _ := gw.Tail(zendStringView(scope), int(limit), 0)
	buf := scopecache.AppendTailResponseJSON(nil, items, truncated, 0)
	return phpStringFromBytes(buf)
}

// --- Writes ----------------------------------------------------------

// scopecache_append — POST /append envelope as a JSON string. `created`
// is always true; carried for write-envelope uniformity.
//
// export_php:function scopecache_append(string $scope, string $id, string $payload): ?string
func scopecache_append(scope *C.zend_string, id *C.zend_string, payload *C.zend_string) unsafe.Pointer {
	gw := defaultSlot.Load()
	if gw == nil {
		return nil
	}
	item := scopecache.Item{
		Scope:   zendStringCopy(scope),
		ID:      zendStringCopy(id),
		Payload: zendStringBytes(payload),
	}
	result, err := gw.Append(item)
	if err != nil {
		return errorEnvelope(err.Error())
	}
	body, mErr := scopecache.MarshalEnvelope(scopecache.AppendResponse{
		OK: true, Created: true, Item: scopecache.NewWriteAck(result),
	})
	if mErr != nil {
		return errorEnvelope("internal: response marshal failed")
	}
	return phpStringFromBytes(body)
}

// scopecache_upsert — POST /upsert envelope. `created` distinguishes
// first-write from in-place replace; seq is preserved on replace.
//
// export_php:function scopecache_upsert(string $scope, string $id, string $payload): ?string
func scopecache_upsert(scope *C.zend_string, id *C.zend_string, payload *C.zend_string) unsafe.Pointer {
	gw := defaultSlot.Load()
	if gw == nil {
		return nil
	}
	item := scopecache.Item{
		Scope:   zendStringCopy(scope),
		ID:      zendStringCopy(id),
		Payload: zendStringBytes(payload),
	}
	result, created, err := gw.Upsert(item)
	if err != nil {
		return errorEnvelope(err.Error())
	}
	body, mErr := scopecache.MarshalEnvelope(scopecache.UpsertResponse{
		OK: true, Created: created, Item: scopecache.NewWriteAck(result),
	})
	if mErr != nil {
		return errorEnvelope("internal: response marshal failed")
	}
	return phpStringFromBytes(body)
}

// scopecache_update — POST /update envelope. `created` is always
// false; `count` is 0 on miss, 1 on hit.
//
// export_php:function scopecache_update(string $scope, string $id, string $payload): ?string
func scopecache_update(scope *C.zend_string, id *C.zend_string, payload *C.zend_string) unsafe.Pointer {
	gw := defaultSlot.Load()
	if gw == nil {
		return nil
	}
	item := scopecache.Item{
		Scope:   zendStringCopy(scope),
		ID:      zendStringCopy(id),
		Payload: zendStringBytes(payload),
	}
	n, err := gw.Update(item)
	if err != nil {
		return errorEnvelope(err.Error())
	}
	body, mErr := scopecache.MarshalEnvelope(scopecache.UpdateResponse{
		OK: true, Created: false, Count: n,
	})
	if mErr != nil {
		return errorEnvelope("internal: response marshal failed")
	}
	return phpStringFromBytes(body)
}

// scopecache_counter_add — POST /counter_add envelope. `created` is
// true on first-touch; `value` is the post-add counter value.
//
// export_php:function scopecache_counter_add(string $scope, string $id, int $by): ?string
func scopecache_counter_add(scope *C.zend_string, id *C.zend_string, by int64) unsafe.Pointer {
	gw := defaultSlot.Load()
	if gw == nil {
		return nil
	}
	value, created, err := gw.CounterAdd(zendStringCopy(scope), zendStringCopy(id), by)
	if err != nil {
		return errorEnvelope(err.Error())
	}
	body, mErr := scopecache.MarshalEnvelope(scopecache.CounterAddResponse{
		OK: true, Created: created, Value: value,
	})
	if mErr != nil {
		return errorEnvelope("internal: response marshal failed")
	}
	return phpStringFromBytes(body)
}

// --- Deletes ---------------------------------------------------------

// scopecache_delete — POST /delete envelope. `count` is 0 or 1.
//
// export_php:function scopecache_delete(string $scope, string $id): ?string
func scopecache_delete(scope *C.zend_string, id *C.zend_string) unsafe.Pointer {
	gw := defaultSlot.Load()
	if gw == nil {
		return nil
	}
	n, err := gw.Delete(zendStringView(scope), zendStringView(id), 0)
	if err != nil {
		return errorEnvelope(err.Error())
	}
	return phpStringFromBytes(mustMarshal(scopecache.DeleteResponse{OK: true, Hit: n > 0, Count: n}))
}

// scopecache_delete_by_seq — POST /delete envelope, addressed by seq.
//
// export_php:function scopecache_delete_by_seq(string $scope, int $seq): ?string
func scopecache_delete_by_seq(scope *C.zend_string, seq int64) unsafe.Pointer {
	gw := defaultSlot.Load()
	if gw == nil {
		return nil
	}
	n, err := gw.Delete(zendStringView(scope), "", uint64(seq))
	if err != nil {
		return errorEnvelope(err.Error())
	}
	return phpStringFromBytes(mustMarshal(scopecache.DeleteResponse{OK: true, Hit: n > 0, Count: n}))
}

// scopecache_delete_up_to — POST /delete_up_to envelope. `count` is
// the number of items released.
//
// export_php:function scopecache_delete_up_to(string $scope, int $max_seq): ?string
func scopecache_delete_up_to(scope *C.zend_string, maxSeq int64) unsafe.Pointer {
	gw := defaultSlot.Load()
	if gw == nil {
		return nil
	}
	n, err := gw.DeleteUpTo(zendStringView(scope), uint64(maxSeq))
	if err != nil {
		return errorEnvelope(err.Error())
	}
	return phpStringFromBytes(mustMarshal(scopecache.DeleteResponse{OK: true, Hit: n > 0, Count: n}))
}

// scopecache_delete_scope — POST /delete_scope envelope. `hit` reflects
// "scope existed pre-call" (empty-but-existing still hits).
//
// export_php:function scopecache_delete_scope(string $scope): ?string
func scopecache_delete_scope(scope *C.zend_string) unsafe.Pointer {
	gw := defaultSlot.Load()
	if gw == nil {
		return nil
	}
	n, hit, err := gw.DeleteScope(zendStringView(scope))
	if err != nil {
		return errorEnvelope(err.Error())
	}
	return phpStringFromBytes(mustMarshal(scopecache.DeleteScopeResponse{OK: true, Hit: hit, Count: n}))
}

// scopecache_wipe — POST /wipe envelope. `scopes` / `items` count what
// was dropped (reserved scopes are dropped + immediately re-created,
// so a fresh-booted wipe still reports scopes=2).
//
// export_php:function scopecache_wipe(): ?string
func scopecache_wipe() unsafe.Pointer {
	gw := defaultSlot.Load()
	if gw == nil {
		return nil
	}
	scopeCount, itemCount, freedBytes := gw.Wipe()
	return phpStringFromBytes(mustMarshal(scopecache.WipeResponse{
		OK: true, Scopes: scopeCount, Items: itemCount, FreedMB: scopecache.MB(freedBytes),
	}))
}

// --- Observability ---------------------------------------------------

// scopecache_stats — GET /stats envelope.
//
// export_php:function scopecache_stats(): ?string
func scopecache_stats() unsafe.Pointer {
	gw := defaultSlot.Load()
	if gw == nil {
		return nil
	}
	st := gw.Stats()
	return phpStringFromBytes(mustMarshal(scopecache.StatsResponse{
		OK:            true,
		Scopes:        st.Scopes,
		Items:         st.Items,
		ApproxStoreMB: st.ApproxStoreMB,
		LastWriteTS:   st.LastWriteTS,
	}))
}

// scopecache_scopelist — GET /scopelist envelope. `prefix` "" = no
// filter; `after` "" = start from beginning; `limit` clamped server-side.
//
// export_php:function scopecache_scopelist(string $prefix, string $after, int $limit): ?string
func scopecache_scopelist(prefix *C.zend_string, after *C.zend_string, limit int64) unsafe.Pointer {
	gw := defaultSlot.Load()
	if gw == nil {
		return nil
	}
	entries, truncated := gw.ScopeList(zendStringView(prefix), zendStringView(after), int(limit))
	buf := scopecache.AppendScopeListResponseJSON(nil, entries, truncated)
	return phpStringFromBytes(buf)
}

// --- Bulk ------------------------------------------------------------

// phpArrayToGroupedItems converts the nested-PHP-array input of
// scopecache_warm / _rebuild into scopecache's map[string][]Item.
// Input shape:
//
//	['scope-a' => [['id' => 'x', 'payload' => '{...}'], ...], 'scope-b' => [...]]
//
// Inner keys: 'payload' required, 'id' optional (missing = seq-only).
// Returns early on the first structural mismatch — Gateway.Warm
// validates atomically before any lock, so a half-converted input
// would produce a useless partial-error response anyway.
//
// frankenphp.GoMap copies all PHP-string bytes via C.GoStringN, so
// the returned Go strings are safe to retain as map keys inside
// scopecache.
func phpArrayToGroupedItems(arr *C.zend_array) (map[string][]scopecache.Item, error) {
	if arr == nil {
		return nil, fmt.Errorf("nil array")
	}
	raw, err := frankenphp.GoMap[[]any](unsafe.Pointer(arr))
	if err != nil {
		return nil, fmt.Errorf("GoMap: %w", err)
	}
	out := make(map[string][]scopecache.Item, len(raw))
	for scope, items := range raw {
		goItems := make([]scopecache.Item, 0, len(items))
		for i, anyItem := range items {
			assoc, ok := anyItem.(frankenphp.AssociativeArray[any])
			if !ok {
				return nil, fmt.Errorf("scope %q item %d: not an associative array (got %T)", scope, i, anyItem)
			}
			payloadAny, hasPayload := assoc.Map["payload"]
			if !hasPayload {
				return nil, fmt.Errorf("scope %q item %d: missing 'payload' key", scope, i)
			}
			payload, ok := payloadAny.(string)
			if !ok {
				return nil, fmt.Errorf("scope %q item %d: 'payload' is not a string (got %T)", scope, i, payloadAny)
			}
			id, _ := assoc.Map["id"].(string) // optional; missing/non-string -> "" (seq-only)
			goItems = append(goItems, scopecache.Item{
				Scope:   scope,
				ID:      id,
				Payload: []byte(payload),
			})
		}
		out[scope] = goItems
	}
	return out, nil
}

// scopecache_warm — POST /warm envelope. Replaces every scope present
// in `grouped`; scopes not in `grouped` are untouched.
//
// export_php:function scopecache_warm(array $grouped): ?string
func scopecache_warm(grouped *C.zend_array) unsafe.Pointer {
	gw := defaultSlot.Load()
	if gw == nil {
		return nil
	}
	goGrouped, err := phpArrayToGroupedItems(grouped)
	if err != nil {
		return errorEnvelope(err.Error())
	}
	n, err := gw.Warm(goGrouped)
	if err != nil {
		return errorEnvelope(err.Error())
	}
	return phpStringFromBytes(mustMarshal(scopecache.WarmResponse{OK: true, Scopes: n}))
}

// scopecache_rebuild — POST /rebuild envelope. Atomically replaces the
// entire user-managed cache state with `grouped`; reserved scopes are
// re-created under the same all-shard write lock. Unlike /warm, scopes
// not in `grouped` are dropped.
//
// export_php:function scopecache_rebuild(array $grouped): ?string
func scopecache_rebuild(grouped *C.zend_array) unsafe.Pointer {
	gw := defaultSlot.Load()
	if gw == nil {
		return nil
	}
	goGrouped, err := phpArrayToGroupedItems(grouped)
	if err != nil {
		return errorEnvelope(err.Error())
	}
	scopes, items, err := gw.Rebuild(goGrouped)
	if err != nil {
		return errorEnvelope(err.Error())
	}
	return phpStringFromBytes(mustMarshal(scopecache.RebuildResponse{OK: true, Scopes: scopes, Items: items}))
}
