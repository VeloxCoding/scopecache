// scopecache_ext.go — FrankenPHP extension that exposes scopecache's
// Go-level *Gateway directly to PHP.
//
// Why this exists: even when scopecache runs inside the same
// FrankenPHP/Caddy binary as the PHP app, PHP→cache calls today still
// go through HTTP (libcurl → loopback TCP → Caddy routing → scopecache
// handler → JSON encode/decode on both sides). The harness measured
// that loopback-HTTP floor at ~3.5 ms for an 11-17 µs cache lookup —
// a ~200× overhead ratio dominated entirely by transport.
//
// This extension removes that transport layer. PHP calls a function
// (scopecache_get, etc.); the FrankenPHP extension generator turns
// that into a C-level PHP_FUNCTION whose body lands in this Go file;
// the Go function calls *Gateway methods in the same process; the
// return value crosses back through cgo as a native PHP value.
//
// Measured wins (54-byte payload, FrankenPHP 8.5 ZTS):
//   - ~750× vs PHP→scopecache over loopback HTTP (persistent curl handle)
//   - ~400× vs PHP→phpredis→Redis (persistent connection)
//   - ~1900× vs PHP→phpredis→Redis (fresh connection per call)
//
// See addons/frankenphp-ext/bench.php for the measurement harness.
//
// Boundary discipline (CLAUDE.md):
//   - This is an addon. The core never changes shape to accommodate it.
//   - PHP and the Caddy module share one *Gateway via the process-wide
//     named registry in gateway_registry.go: the caddymodule registers
//     under "default" during Provision(), this extension looks it up at
//     every call. Same data both ways; same Caddyfile config; same
//     /stats output. No second hidden cache.
//
// Hot-path discipline:
//   - The Gateway pointer is looked up once at package init via
//     scopecache.LookupGatewaySlot, then read per call with a single
//     atomic Load (~1-2 ns) instead of going through the registry's
//     RLock + map lookup (~30-40 ns). Caddy reload swaps the slot
//     contents atomically — no invalidation logic needed here.
//   - For READ-ONLY paths (scopecache_get, scopecache_tail), scope/id
//     cross C→Go as zero-copy unsafe.String views into the zend_string
//     bytes (zendStringView). Safe because the Gateway consumes them
//     synchronously and retains no references — see store.get and
//     buffer_read.getByID.
//   - For WRITE paths (scopecache_append), scope/id MUST be copied
//     (zendStringCopy) because they become permanent map keys inside
//     scopecache. The zero-copy alias would point to PHP-arena memory
//     that PHP frees at request end, leaving the map indexed by
//     garbage.
//   - The returned payload skips the []byte→string→zend_string detour
//     that frankenphp.PHPString takes, directly emalloc'ing a fresh
//     zend_string in the PHP request arena via phpStringFromBytes.
//   - Input payloads on the write path use zendStringBytes (alias);
//     safe because Gateway.Append's cloneItemPayload copies them
//     synchronously at the boundary.

package scopecache_ext

/*
#include <php.h>

// Inline cgo-visible wrappers around PHP's ZVAL_* macros and a few
// utility helpers. cgo cannot call C macros directly, so each macro
// gets a tiny static-inline trampoline. Same pattern frankenphp's
// types.go uses internally (their __zval_*__ helpers), kept under
// our own naming so it is clear at the call site that the symbol
// lives in this addon, not in upstream frankenphp. <php.h> is the
// master include that pulls in zend_types.h (zval, ZVAL_*),
// zend_string.h (zend_string_init), zend_hash.h
// (zend_hash_next_index_insert, zend_new_array) and everything
// else PHP-API-side.
static inline void sc_zval_str(zval *zv, zend_string *s) {
    ZVAL_STR(zv, s);
}
static inline void sc_zval_empty_string(zval *zv) {
    ZVAL_EMPTY_STRING(zv);
}
static inline void sc_zval_bool(zval *zv, int b) {
    ZVAL_BOOL(zv, b);
}
static inline void sc_zval_long(zval *zv, zend_long n) {
    ZVAL_LONG(zv, n);
}
static inline void sc_zval_double(zval *zv, double d) {
    ZVAL_DOUBLE(zv, d);
}
static inline void sc_zval_null(zval *zv) {
    ZVAL_NULL(zv);
}
static inline void sc_zval_arr(zval *zv, zend_array *a) {
    ZVAL_ARR(zv, a);
}
// zend_new_array itself is a macro expanding to _zend_new_array(size);
// cgo cannot call macros, so trampoline it.
static inline zend_array *sc_zend_new_array(uint32_t size) {
    return zend_new_array(size);
}
// String-keyed insert — used by phpAssocAdd*. zend_hash_str_add_new is
// the no-collision variant (faster; we always build with unique keys).
// On collision it returns NULL; we ignore the return because our
// envelope shapes are constructed key-by-key and never re-insert.
static inline zval *sc_hash_str_add(zend_array *ht, const char *key, size_t key_len, zval *zv) {
    return zend_hash_str_add_new(ht, key, key_len, zv);
}
*/
import "C"

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"unsafe"

	"github.com/VeloxCoding/scopecache"
	"github.com/dunglas/frankenphp"
)

// defaultSlot is the atomic *Gateway slot for the "default" name,
// cached at package init. LookupGatewaySlot lazily creates the slot
// on first reference, so this is safe even if package init runs
// before the caddymodule's Provision() — the slot holds nil until
// Register fires, and our scopecache_get already handles that case.
var defaultSlot *atomic.Pointer[scopecache.Gateway] = scopecache.LookupGatewaySlot("default")

// zendStringView returns a Go string aliasing the zend_string's byte
// storage — zero copies, zero allocations. Valid only for the
// duration of the calling PHP_FUNCTION: the underlying zend_string is
// PHP's emalloc'd request memory and lives at least as long as the
// request, but callers must NOT retain the returned string past their
// own cgo-call return.
//
// scopecache.GetByID satisfies this constraint — scope/id are used as
// synchronous map keys and never stored. Cross-checked in
// [store.go](../../store.go) and [buffer_read.go](../../buffer_read.go).
func zendStringView(s *C.zend_string) string {
	if s == nil {
		return ""
	}
	return unsafe.String((*byte)(unsafe.Pointer(&s.val)), int(s.len))
}

// zendStringBytes returns a []byte view over a zend_string, no copy.
// Same lifetime contract as zendStringView. Used for payload bytes
// that scopecache.Append immediately clones at the Gateway boundary,
// so retention by scopecache is not an issue.
func zendStringBytes(s *C.zend_string) []byte {
	if s == nil {
		return nil
	}
	return unsafe.Slice((*byte)(unsafe.Pointer(&s.val)), int(s.len))
}

// zendStringCopy returns a Go string with a fresh copy of the
// zend_string bytes. Safe for permanent retention — use this when
// the resulting string will outlive the calling PHP_FUNCTION (e.g.
// becomes a map key in scopecache's internal structures).
//
// scopecache_append needs this for both scope and id because those
// strings become permanent map keys: s.shards[X].scopes[scope] and
// b.byID[id]. zendStringView aliases would point to PHP-arena
// memory that PHP frees once the request ends, leaving the map
// indexed by garbage. scopecache_get / scopecache_tail can use
// zendStringView because they only LOOK UP, never STORE.
func zendStringCopy(s *C.zend_string) string {
	if s == nil {
		return ""
	}
	return C.GoStringN((*C.char)(unsafe.Pointer(&s.val)), C.int(s.len))
}

// phpStringFromBytes emalloc's a fresh zend_string in the PHP request
// arena and copies the given bytes in, skipping the []byte→string→
// zend_string detour frankenphp.PHPString takes. The returned pointer
// is owned by the wrapper's RETURN_STR — PHP frees it on request
// shutdown.
//
// Empty input returns nil; combined with our build-time wrapper
// patch (RETURN_EMPTY_STRING→RETURN_NULL), PHP sees null. NB: this
// means an item legitimately holding a 0-byte payload would be
// reported as a miss. Safe today because scopecache validation
// requires non-empty JSON; documented here so a future validation
// loosening upstream doesn't silently break this extension.
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

// --- PHP-array builders ----------------------------------------------
//
// Every public PHP function mirrors its HTTP-wire counterpart by
// returning a PHP associative array with the same key set the HTTP
// envelope carries (response_types.go in the cache core is the source
// of truth). To avoid the "marshal-to-JSON in Go, decode in PHP"
// round-trip, the builders write into PHP HashTables directly via the
// cgo trampolines above.
//
// Convention used by phpAssocAdd*:
//   - `key` is a Go string; sc_hash_str_add copies it into the
//     HashTable, so the underlying Go memory does not need to live
//     past the call.
//   - One stack-allocated zval is reused per insert; zend_hash_*
//     copies the zval contents on insert.
//   - The returned *C.zend_array is owned by the PHP request arena
//     (the wrapper's RETURN_ARR transfers ownership to PHP).

func phpAssocNew(size int) *C.zend_array {
	return C.sc_zend_new_array(C.uint32_t(size))
}

func phpHashStrAdd(arr *C.zend_array, key string, zv *C.zval) {
	if len(key) == 0 {
		return
	}
	C.sc_hash_str_add(
		arr,
		(*C.char)(unsafe.Pointer(unsafe.StringData(key))),
		C.size_t(len(key)),
		zv,
	)
}

func phpAssocAddBool(arr *C.zend_array, key string, val bool) {
	var zv C.zval
	var b C.int
	if val {
		b = 1
	}
	C.sc_zval_bool(&zv, b)
	phpHashStrAdd(arr, key, &zv)
}

func phpAssocAddLong(arr *C.zend_array, key string, val int64) {
	var zv C.zval
	C.sc_zval_long(&zv, C.zend_long(val))
	phpHashStrAdd(arr, key, &zv)
}

func phpAssocAddDouble(arr *C.zend_array, key string, val float64) {
	var zv C.zval
	C.sc_zval_double(&zv, C.double(val))
	phpHashStrAdd(arr, key, &zv)
}

func phpAssocAddNull(arr *C.zend_array, key string) {
	var zv C.zval
	C.sc_zval_null(&zv)
	phpHashStrAdd(arr, key, &zv)
}

func phpAssocAddString(arr *C.zend_array, key string, val string) {
	var zv C.zval
	if len(val) == 0 {
		C.sc_zval_empty_string(&zv)
	} else {
		zs := C.zend_string_init(
			(*C.char)(unsafe.Pointer(unsafe.StringData(val))),
			C.size_t(len(val)),
			C._Bool(false),
		)
		C.sc_zval_str(&zv, zs)
	}
	phpHashStrAdd(arr, key, &zv)
}

func phpAssocAddArray(arr *C.zend_array, key string, val *C.zend_array) {
	var zv C.zval
	C.sc_zval_arr(&zv, val)
	phpHashStrAdd(arr, key, &zv)
}

func phpAssocAddZval(arr *C.zend_array, key string, zv *C.zval) {
	phpHashStrAdd(arr, key, zv)
}

// payloadToZval decodes a stored item's JSON payload bytes and writes
// the resulting PHP value into zv. Mirrors what `json_decode($body, true)`
// would produce in PHP on the same bytes — object → assoc array, array →
// packed array, number → int when no decimal/exponent in source else
// float, string/bool/null map directly.
//
// Token-walked (not json.Unmarshal-into-any) so JSON-object key order
// is preserved on the way into the PHP array. PHP's strict-equality
// operator on assoc arrays compares insertion order; a decoder that
// went through map[string]any would lose source order to Go's map-
// iteration randomisation, breaking the `===` parity we promise.
//
// json.Decoder.UseNumber() preserves the int-vs-float distinction the
// JSON source had; the default float64 decode would silently widen
// every integer payload (`42` → float 42.0 → PHP float when emitted),
// breaking the json_decode parity too.
//
// Empty input (defensive — validatePayload rejects this upstream)
// writes PHP null.
func payloadToZval(payload json.RawMessage, zv *C.zval) {
	if len(payload) == 0 {
		C.sc_zval_null(zv)
		return
	}
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	if err := decodeNextToZval(dec, zv); err != nil {
		C.sc_zval_null(zv)
	}
}

// decodeNextToZval pulls one JSON value (scalar, object, or array)
// from dec and writes it into zv. Recurses into nested structures
// via the same function, consuming the matching closing delimiter
// before returning.
func decodeNextToZval(dec *json.Decoder, zv *C.zval) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	return tokenToZval(dec, tok, zv)
}

func tokenToZval(dec *json.Decoder, tok json.Token, zv *C.zval) error {
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			arr := C.sc_zend_new_array(C.uint32_t(8))
			for dec.More() {
				keyTok, err := dec.Token()
				if err != nil {
					return err
				}
				key, ok := keyTok.(string)
				if !ok {
					return fmt.Errorf("payload decode: object key is not a string (got %T)", keyTok)
				}
				var valZv C.zval
				if err := decodeNextToZval(dec, &valZv); err != nil {
					return err
				}
				phpHashStrAdd(arr, key, &valZv)
			}
			if _, err := dec.Token(); err != nil { // consume '}'
				return err
			}
			C.sc_zval_arr(zv, arr)
			return nil
		case '[':
			arr := C.sc_zend_new_array(C.uint32_t(8))
			for dec.More() {
				var elemZv C.zval
				if err := decodeNextToZval(dec, &elemZv); err != nil {
					return err
				}
				C.zend_hash_next_index_insert(arr, &elemZv)
			}
			if _, err := dec.Token(); err != nil { // consume ']'
				return err
			}
			C.sc_zval_arr(zv, arr)
			return nil
		default:
			return fmt.Errorf("payload decode: unexpected delimiter %v", t)
		}
	case nil:
		C.sc_zval_null(zv)
		return nil
	case bool:
		var b C.int
		if t {
			b = 1
		}
		C.sc_zval_bool(zv, b)
		return nil
	case json.Number:
		s := string(t)
		if strings.ContainsAny(s, ".eE") {
			if f, err := strconv.ParseFloat(s, 64); err == nil {
				C.sc_zval_double(zv, C.double(f))
				return nil
			}
		}
		if i, err := strconv.ParseInt(s, 10, 64); err == nil {
			C.sc_zval_long(zv, C.zend_long(i))
			return nil
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			C.sc_zval_double(zv, C.double(f))
			return nil
		}
		C.sc_zval_null(zv)
		return nil
	case string:
		if len(t) == 0 {
			C.sc_zval_empty_string(zv)
		} else {
			zs := C.zend_string_init(
				(*C.char)(unsafe.Pointer(unsafe.StringData(t))),
				C.size_t(len(t)),
				C._Bool(false),
			)
			C.sc_zval_str(zv, zs)
		}
		return nil
	default:
		C.sc_zval_null(zv)
		return nil
	}
}

// buildItemAssoc constructs the per-item PHP-array used inside the
// `item` key on /get and as each element of `items` on /head and
// /tail. Includes the (decoded) payload. Mirrors Item.MarshalJSON's
// wire shape (scope/id/seq/ts + payload-or-event) from §4.2 of the
// RFC.
func buildItemAssoc(item scopecache.Item) *C.zend_array {
	arr := phpAssocNew(5)
	phpAssocAddString(arr, "scope", item.Scope)
	if item.ID == "" {
		phpAssocAddNull(arr, "id")
	} else {
		phpAssocAddString(arr, "id", item.ID)
	}
	phpAssocAddLong(arr, "seq", int64(item.Seq))
	phpAssocAddLong(arr, "ts", item.Ts)

	// payload renames to "event" for items in the reserved _events
	// scope — see types.go Item.MarshalJSON for the rationale.
	payloadKey := "payload"
	if item.Scope == scopecache.EventsScopeName {
		payloadKey = "event"
	}
	var zv C.zval
	payloadToZval(item.Payload, &zv)
	phpAssocAddZval(arr, payloadKey, &zv)
	return arr
}

// buildWriteAckAssoc constructs the `item` sub-array for /append and
// /upsert responses. Same shape as buildItemAssoc minus the payload —
// the client just supplied it on the way in, so echoing it would
// double the wire cost. Matches writeAck in handlers_write.go.
func buildWriteAckAssoc(item scopecache.Item) *C.zend_array {
	arr := phpAssocNew(4)
	phpAssocAddString(arr, "scope", item.Scope)
	if item.ID == "" {
		phpAssocAddNull(arr, "id")
	} else {
		phpAssocAddString(arr, "id", item.ID)
	}
	phpAssocAddLong(arr, "seq", int64(item.Seq))
	phpAssocAddLong(arr, "ts", item.Ts)
	return arr
}

// buildItemsPackedArray constructs the inner `items` packed-array of
// /head and /tail responses — every element is the full item shape
// from buildItemAssoc (scope/id/seq/ts/payload). Returns an empty
// packed array when `items` is empty.
func buildItemsPackedArray(items []scopecache.Item) *C.zend_array {
	arr := C.sc_zend_new_array(C.uint32_t(len(items)))
	var zv C.zval
	for i := range items {
		C.sc_zval_arr(&zv, buildItemAssoc(items[i]))
		C.zend_hash_next_index_insert(arr, &zv)
	}
	return arr
}

// buildItemsEnvelope assembles the /head + /tail success envelope.
// withOffset toggles the /tail-only `offset` field; offsetVal is
// the value to emit when withOffset is true (always 0 today, since
// scopecache_tail does not expose the offset parameter).
func buildItemsEnvelope(hit bool, items []scopecache.Item, truncated bool, withOffset bool, offsetVal int64) *C.zend_array {
	size := 5
	if withOffset {
		size++
	}
	arr := phpAssocNew(size)
	phpAssocAddBool(arr, "ok", true)
	phpAssocAddBool(arr, "hit", hit)
	phpAssocAddLong(arr, "count", int64(len(items)))
	if withOffset {
		phpAssocAddLong(arr, "offset", offsetVal)
	}
	phpAssocAddBool(arr, "truncated", truncated)
	phpAssocAddArray(arr, "items", buildItemsPackedArray(items))
	return arr
}

// buildReservedScopesArray builds the inner packed array of
// /stats's `reserved_scopes` block — one row per reserved scope
// with the six-field slim shape (no last_access_ts / read_count_total
// since reserved scopes are infrastructure, not user-facing reads).
func buildReservedScopesArray(rows []scopecache.ReservedScopeEntry) *C.zend_array {
	arr := C.sc_zend_new_array(C.uint32_t(len(rows)))
	var zv C.zval
	for i := range rows {
		row := phpAssocNew(6)
		phpAssocAddString(row, "scope", rows[i].Scope)
		phpAssocAddLong(row, "item_count", int64(rows[i].ItemCount))
		phpAssocAddLong(row, "last_seq", int64(rows[i].LastSeq))
		phpAssocAddDouble(row, "approx_scope_mb", float64(rows[i].ApproxScopeMB)/1048576.0)
		phpAssocAddLong(row, "created_ts", rows[i].CreatedTS)
		phpAssocAddLong(row, "last_write_ts", rows[i].LastWriteTS)
		C.sc_zval_arr(&zv, row)
		C.zend_hash_next_index_insert(arr, &zv)
	}
	return arr
}

// buildScopeListPackedArray builds the inner packed array of
// /scopelist's `scopes` field — full eight-field row per scope
// including the read-bookkeeping signals (last_access_ts,
// read_count_total) that /stats's reserved_scopes block omits.
func buildScopeListPackedArray(rows []scopecache.ScopeListEntry) *C.zend_array {
	arr := C.sc_zend_new_array(C.uint32_t(len(rows)))
	var zv C.zval
	for i := range rows {
		row := phpAssocNew(8)
		phpAssocAddString(row, "scope", rows[i].Scope)
		phpAssocAddLong(row, "item_count", int64(rows[i].ItemCount))
		phpAssocAddLong(row, "last_seq", int64(rows[i].LastSeq))
		phpAssocAddDouble(row, "approx_scope_mb", float64(rows[i].ApproxScopeMB)/1048576.0)
		phpAssocAddLong(row, "created_ts", rows[i].CreatedTS)
		phpAssocAddLong(row, "last_write_ts", rows[i].LastWriteTS)
		phpAssocAddLong(row, "last_access_ts", rows[i].LastAccessTS)
		phpAssocAddLong(row, "read_count_total", int64(rows[i].ReadCountTotal))
		C.sc_zval_arr(&zv, row)
		C.zend_hash_next_index_insert(arr, &zv)
	}
	return arr
}

// buildHitCountEnvelope returns the {ok, hit, count} shape shared by
// /delete and /delete_up_to. `hit` is derived as `count > 0`.
func buildHitCountEnvelope(count int) *C.zend_array {
	arr := phpAssocNew(3)
	phpAssocAddBool(arr, "ok", true)
	phpAssocAddBool(arr, "hit", count > 0)
	phpAssocAddLong(arr, "count", int64(count))
	return arr
}

// buildErrorEnvelope returns the PHP-array form of the HTTP error
// envelope `{ok: false, error: msg}`. Used when scopecache returns an
// error the extension cannot otherwise represent in a typed-success
// envelope (validation failure on input, capacity exceeded, etc.).
func buildErrorEnvelope(msg string) *C.zend_array {
	arr := phpAssocNew(2)
	phpAssocAddBool(arr, "ok", false)
	phpAssocAddString(arr, "error", msg)
	return arr
}

// errorEnvelopePtr is the unsafe.Pointer form of buildErrorEnvelope,
// shaped for direct return from the //export_php functions.
func errorEnvelopePtr(msg string) unsafe.Pointer {
	return unsafe.Pointer(buildErrorEnvelope(msg))
}

// errorMsg extracts a stable error string from a scopecache Gateway
// error. ErrInvalidInput's wrapped message goes through verbatim;
// other errors (capacity, conflict) emit their .Error() string.
// Mirrors what the HTTP error responses surface.
func errorMsg(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// scopecache_get returns the same JSON envelope GET /get serves over
// HTTP, but as a native PHP associative array — no json_decode needed.
//
// Hit:
//
//	['ok' => true, 'hit' => true, 'count' => 1,
//	 'item' => ['scope' => '...', 'id' => '...' | null, 'seq' => N,
//	            'ts' => N, 'payload' => mixed]]
//
// Miss:
//
//	['ok' => true, 'hit' => false, 'count' => 0, 'item' => null]
//
// `payload` is decoded the same way `json_decode($body, true)` would
// decode it from the HTTP response: object → assoc array, array →
// packed array, number → int or float, etc.
//
// Returns null only when no scopecache caddymodule is loaded in this
// binary (Provision never registered a *Gateway). An operator seeing
// null from this function should check that the Caddyfile has a
// `scopecache {}` block.
//
// export_php:function scopecache_get(string $scope, string $id): ?array
func scopecache_get(scope *C.zend_string, id *C.zend_string) unsafe.Pointer {
	gw := defaultSlot.Load()
	if gw == nil {
		return nil
	}
	item, found := gw.GetByID(zendStringView(scope), zendStringView(id))
	arr := phpAssocNew(4)
	phpAssocAddBool(arr, "ok", true)
	phpAssocAddBool(arr, "hit", found)
	if found {
		phpAssocAddLong(arr, "count", 1)
		phpAssocAddArray(arr, "item", buildItemAssoc(item))
	} else {
		phpAssocAddLong(arr, "count", 0)
		phpAssocAddNull(arr, "item")
	}
	return unsafe.Pointer(arr)
}

// scopecache_tail returns the GET /tail envelope as a PHP array.
// `offset` is always 0 (single-shot tail; pagination by offset is not
// exposed in the PHP signature).
//
// Hit:
//
//	['ok' => true, 'hit' => true, 'count' => N, 'offset' => 0,
//	 'truncated' => bool,
//	 'items' => [ ['scope' => ..., 'id' => ... | null, 'seq' => ..., 'ts' => ..., 'payload' => mixed], ... ]]
//
// Miss (scope does not exist):
//
//	['ok' => true, 'hit' => false, 'count' => 0, 'offset' => 0,
//	 'truncated' => false, 'items' => []]
//
// Returns null only when no caddymodule is loaded.
//
// export_php:function scopecache_tail(string $scope, int $limit): ?array
func scopecache_tail(scope *C.zend_string, limit int64) unsafe.Pointer {
	gw := defaultSlot.Load()
	if gw == nil {
		return nil
	}
	items, truncated, found := gw.Tail(zendStringView(scope), int(limit), 0)
	return unsafe.Pointer(buildItemsEnvelope(found, items, truncated, true, 0))
}

// scopecache_head returns the GET /head envelope as a PHP array.
// Companion to scopecache_tail for forward cursor-based reads.
//
// Hit:
//
//	['ok' => true, 'hit' => true, 'count' => N,
//	 'truncated' => bool, 'items' => [ ... ]]
//
// Miss:
//
//	['ok' => true, 'hit' => false, 'count' => 0,
//	 'truncated' => false, 'items' => []]
//
// `after_seq` filter: 0 starts from the beginning; otherwise returns
// items with seq strictly greater than `after_seq`. Each item carries
// its decoded payload as in scopecache_get / scopecache_tail.
//
// export_php:function scopecache_head(string $scope, int $after_seq, int $limit): ?array
func scopecache_head(scope *C.zend_string, afterSeq int64, limit int64) unsafe.Pointer {
	gw := defaultSlot.Load()
	if gw == nil {
		return nil
	}
	items, truncated, found := gw.Head(zendStringView(scope), uint64(afterSeq), int(limit))
	return unsafe.Pointer(buildItemsEnvelope(found, items, truncated, false, 0))
}

// scopecache_get_by_seq looks up a single item by scope + seq. Same
// envelope shape as scopecache_get, just addressed by the cache-
// assigned seq instead of the client-supplied id.
//
// export_php:function scopecache_get_by_seq(string $scope, int $seq): ?array
func scopecache_get_by_seq(scope *C.zend_string, seq int64) unsafe.Pointer {
	gw := defaultSlot.Load()
	if gw == nil {
		return nil
	}
	item, found := gw.GetBySeq(zendStringView(scope), uint64(seq))
	arr := phpAssocNew(4)
	phpAssocAddBool(arr, "ok", true)
	phpAssocAddBool(arr, "hit", found)
	if found {
		phpAssocAddLong(arr, "count", 1)
		phpAssocAddArray(arr, "item", buildItemAssoc(item))
	} else {
		phpAssocAddLong(arr, "count", 0)
		phpAssocAddNull(arr, "item")
	}
	return unsafe.Pointer(arr)
}

// scopecache_render_by_id returns the rendered (HTTP-wire-shape)
// bytes for an item — same content as the body of GET /render?... in
// the HTTP API. For pre-rendered JSON-string payloads this is the
// shortcut path that bypasses re-serialisation; for other payload
// shapes it re-emits the canonical JSON.
//
// export_php:function scopecache_render_by_id(string $scope, string $id): ?string
func scopecache_render_by_id(scope *C.zend_string, id *C.zend_string) unsafe.Pointer {
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

// scopecache_render_by_seq returns the rendered bytes for an item
// addressed by scope + seq.
//
// export_php:function scopecache_render_by_seq(string $scope, int $seq): ?string
func scopecache_render_by_seq(scope *C.zend_string, seq int64) unsafe.Pointer {
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

// scopecache_append returns the POST /append envelope as a PHP array.
//
// Success:
//
//	['ok' => true, 'created' => true,
//	 'item' => ['scope' => ..., 'id' => ... | null, 'seq' => N, 'ts' => N]]
//
// Error (capacity, duplicate id, validation failure):
//
//	['ok' => false, 'error' => '<message>']
//
// `created` is always true on /append (the endpoint never replaces)
// — emitted for uniformity with /upsert and /counter_add. The `item`
// sub-array deliberately omits `payload` (the client just supplied
// it, doubling the wire cost would echo it back).
//
// Scope and id are deep-copied at the cgo boundary because they
// become permanent map keys inside scopecache; a zero-copy alias
// would point to PHP-arena memory freed at request end.
//
// export_php:function scopecache_append(string $scope, string $id, string $payload): ?array
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
		return errorEnvelopePtr(errorMsg(err))
	}
	arr := phpAssocNew(3)
	phpAssocAddBool(arr, "ok", true)
	phpAssocAddBool(arr, "created", true)
	phpAssocAddArray(arr, "item", buildWriteAckAssoc(result))
	return unsafe.Pointer(arr)
}

// scopecache_upsert returns the POST /upsert envelope as a PHP array.
//
// Success:
//
//	['ok' => true, 'created' => bool,
//	 'item' => ['scope' => ..., 'id' => ..., 'seq' => N, 'ts' => N]]
//
// `created` is true on first-write of (scope, id), false when an
// existing item was replaced. On replace, `seq` keeps its original
// value; on create, `seq` is freshly assigned. `ts` is always
// refreshed.
//
// export_php:function scopecache_upsert(string $scope, string $id, string $payload): ?array
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
		return errorEnvelopePtr(errorMsg(err))
	}
	arr := phpAssocNew(3)
	phpAssocAddBool(arr, "ok", true)
	phpAssocAddBool(arr, "created", created)
	phpAssocAddArray(arr, "item", buildWriteAckAssoc(result))
	return unsafe.Pointer(arr)
}

// scopecache_update returns the POST /update envelope as a PHP array.
//
// Success:
//
//	['ok' => true, 'created' => false, 'count' => 0 | 1]
//
// `created` is always false on /update (the endpoint never spawns
// new items) — carried for write-envelope uniformity. `count` is
// the number of items modified: 0 on miss, 1 on hit.
//
// export_php:function scopecache_update(string $scope, string $id, string $payload): ?array
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
		return errorEnvelopePtr(errorMsg(err))
	}
	arr := phpAssocNew(3)
	phpAssocAddBool(arr, "ok", true)
	phpAssocAddBool(arr, "created", false)
	phpAssocAddLong(arr, "count", int64(n))
	return unsafe.Pointer(arr)
}

// scopecache_counter_add returns the POST /counter_add envelope as
// a PHP array.
//
// Success:
//
//	['ok' => true, 'created' => bool, 'value' => N]
//
// `created` is true on first-touch (the counter was just spawned
// with value `by`); false when an existing counter was incremented
// in place. `value` is the post-add counter value.
//
// export_php:function scopecache_counter_add(string $scope, string $id, int $by): ?array
func scopecache_counter_add(scope *C.zend_string, id *C.zend_string, by int64) unsafe.Pointer {
	gw := defaultSlot.Load()
	if gw == nil {
		return nil
	}
	value, created, err := gw.CounterAdd(zendStringCopy(scope), zendStringCopy(id), by)
	if err != nil {
		return errorEnvelopePtr(errorMsg(err))
	}
	arr := phpAssocNew(3)
	phpAssocAddBool(arr, "ok", true)
	phpAssocAddBool(arr, "created", created)
	phpAssocAddLong(arr, "value", value)
	return unsafe.Pointer(arr)
}

// scopecache_delete returns the POST /delete envelope as a PHP array.
//
// Success:
//
//	['ok' => true, 'hit' => bool, 'count' => 0 | 1]
//
// `hit` is `count > 0`. `count` is always 0 or 1 since id is
// unique-in-scope. A 409 (scope detached) returns the error envelope.
//
// export_php:function scopecache_delete(string $scope, string $id): ?array
func scopecache_delete(scope *C.zend_string, id *C.zend_string) unsafe.Pointer {
	gw := defaultSlot.Load()
	if gw == nil {
		return nil
	}
	n, err := gw.Delete(zendStringView(scope), zendStringView(id), 0)
	if err != nil {
		return errorEnvelopePtr(errorMsg(err))
	}
	return unsafe.Pointer(buildHitCountEnvelope(n))
}

// scopecache_delete_by_seq returns the POST /delete envelope (seq
// addressing variant) as a PHP array. Same shape as
// scopecache_delete.
//
// export_php:function scopecache_delete_by_seq(string $scope, int $seq): ?array
func scopecache_delete_by_seq(scope *C.zend_string, seq int64) unsafe.Pointer {
	gw := defaultSlot.Load()
	if gw == nil {
		return nil
	}
	n, err := gw.Delete(zendStringView(scope), "", uint64(seq))
	if err != nil {
		return errorEnvelopePtr(errorMsg(err))
	}
	return unsafe.Pointer(buildHitCountEnvelope(n))
}

// scopecache_delete_up_to returns the POST /delete_up_to envelope as
// a PHP array. Same shape as scopecache_delete — `hit` reflects
// `count > 0`, `count` is the number of items actually removed.
//
// export_php:function scopecache_delete_up_to(string $scope, int $max_seq): ?array
func scopecache_delete_up_to(scope *C.zend_string, maxSeq int64) unsafe.Pointer {
	gw := defaultSlot.Load()
	if gw == nil {
		return nil
	}
	n, err := gw.DeleteUpTo(zendStringView(scope), uint64(maxSeq))
	if err != nil {
		return errorEnvelopePtr(errorMsg(err))
	}
	return unsafe.Pointer(buildHitCountEnvelope(n))
}

// scopecache_delete_scope returns the POST /delete_scope envelope
// as a PHP array.
//
// Success:
//
//	['ok' => true, 'hit' => bool, 'count' => N]
//
// `hit` is true when the scope existed pre-call (an
// existing-but-empty scope still hits). `count` is the number of
// items the scope held at deletion time. Reserved scopes
// (`_events`, `_inbox`) return the error envelope.
//
// export_php:function scopecache_delete_scope(string $scope): ?array
func scopecache_delete_scope(scope *C.zend_string) unsafe.Pointer {
	gw := defaultSlot.Load()
	if gw == nil {
		return nil
	}
	n, hit, err := gw.DeleteScope(zendStringView(scope))
	if err != nil {
		return errorEnvelopePtr(errorMsg(err))
	}
	arr := phpAssocNew(3)
	phpAssocAddBool(arr, "ok", true)
	phpAssocAddBool(arr, "hit", hit)
	phpAssocAddLong(arr, "count", int64(n))
	return unsafe.Pointer(arr)
}

// scopecache_wipe returns the POST /wipe envelope as a PHP array.
//
// Success:
//
//	['ok' => true, 'scopes' => N, 'items' => M, 'freed_mb' => F]
//
// `scopes` and `items` count what was dropped — including the two
// reserved scopes that the cache immediately re-creates under the
// same all-shard write lock (so a freshly-booted store wiped
// immediately reports `scopes => 2`, not 0). `freed_mb` is the
// bytes returned to the store-wide budget, in MiB.
//
// Use with care: equivalent to POST /wipe in the HTTP API.
//
// export_php:function scopecache_wipe(): ?array
func scopecache_wipe() unsafe.Pointer {
	gw := defaultSlot.Load()
	if gw == nil {
		return nil
	}
	scopeCount, itemCount, freedBytes := gw.Wipe()
	arr := phpAssocNew(4)
	phpAssocAddBool(arr, "ok", true)
	phpAssocAddLong(arr, "scopes", int64(scopeCount))
	phpAssocAddLong(arr, "items", int64(itemCount))
	phpAssocAddDouble(arr, "freed_mb", float64(freedBytes)/1048576.0)
	return unsafe.Pointer(arr)
}

// scopecache_stats returns the GET /stats envelope as a PHP array —
// identical shape to what HTTP serves, native PHP types throughout.
//
//	['ok' => true,
//	 'scopes' => N, 'items' => M, 'approx_store_mb' => F,
//	 'last_write_ts' => N, 'events_drops_total' => N,
//	 'reserved_scopes' => [
//	    ['scope' => '_events', 'item_count' => N, 'last_seq' => N,
//	     'approx_scope_mb' => F, 'created_ts' => N, 'last_write_ts' => N],
//	    ['scope' => '_inbox',  ...]
//	 ]]
//
// export_php:function scopecache_stats(): ?array
func scopecache_stats() unsafe.Pointer {
	gw := defaultSlot.Load()
	if gw == nil {
		return nil
	}
	st := gw.Stats()
	arr := phpAssocNew(7)
	phpAssocAddBool(arr, "ok", true)
	phpAssocAddLong(arr, "scopes", int64(st.Scopes))
	phpAssocAddLong(arr, "items", int64(st.Items))
	phpAssocAddDouble(arr, "approx_store_mb", float64(st.ApproxStoreMB)/1048576.0)
	phpAssocAddLong(arr, "last_write_ts", st.LastWriteTS)
	phpAssocAddLong(arr, "events_drops_total", st.EventsDropsTotal)
	phpAssocAddArray(arr, "reserved_scopes", buildReservedScopesArray(st.ReservedScopes))
	return unsafe.Pointer(arr)
}

// scopecache_scopelist returns the GET /scopelist envelope as a PHP
// array — paginated per-scope detail rows in alphabetical order.
//
//	['ok' => true, 'hit' => bool, 'count' => N, 'truncated' => bool,
//	 'scopes' => [
//	    ['scope' => '...', 'item_count' => N, 'last_seq' => N,
//	     'approx_scope_mb' => F, 'created_ts' => N, 'last_write_ts' => N,
//	     'last_access_ts' => N, 'read_count_total' => N], ...
//	 ]]
//
// Params: `prefix` "" = no filter; `after` "" = start from beginning;
// `limit` = page size (clamped server-side).
//
// export_php:function scopecache_scopelist(string $prefix, string $after, int $limit): ?array
func scopecache_scopelist(prefix *C.zend_string, after *C.zend_string, limit int64) unsafe.Pointer {
	gw := defaultSlot.Load()
	if gw == nil {
		return nil
	}
	entries, truncated := gw.ScopeList(zendStringView(prefix), zendStringView(after), int(limit))
	arr := phpAssocNew(5)
	phpAssocAddBool(arr, "ok", true)
	phpAssocAddBool(arr, "hit", len(entries) > 0)
	phpAssocAddLong(arr, "count", int64(len(entries)))
	phpAssocAddBool(arr, "truncated", truncated)
	phpAssocAddArray(arr, "scopes", buildScopeListPackedArray(entries))
	return unsafe.Pointer(arr)
}

// phpArrayToGroupedItems converts the nested PHP array shape used by
// scopecache_warm / scopecache_rebuild into scopecache's
// map[string][]Item input. PHP shape:
//
//	['scope-a' => [['id' => 'x', 'payload' => '{...}'], ...], 'scope-b' => [...]]
//
// Inner item keys: 'payload' required (string); 'id' optional
// (missing/empty = seq-only item). The outer key fills Item.Scope —
// any inner 'scope' field is ignored so callers cannot diverge from
// the bulk's implicit grouping.
//
// Returns nil + error on any structural mismatch. Partial conversion
// is never useful: Gateway.Warm/Rebuild validates the full input
// atomically before any shard lock, so half-converted state would
// produce a 0-return anyway. Returning early on the first bad item
// also keeps the diagnostic clean.
//
// Lifetime safety: frankenphp.GoMap copies all PHP-string bytes via
// GoString (C.GoStringN), so the returned Go strings are Go-owned
// and safe to retain in scopecache's internal map keys — no UAF
// concern like pitfall #12.
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

// scopecache_warm returns the POST /warm envelope as a PHP array.
// Replaces the contents of every scope present in `grouped`; scopes
// NOT in `grouped` are left untouched.
//
// Success:
//
//	['ok' => true, 'scopes' => N]
//
// `scopes` is the number of distinct scopes the call rewrote.
// Reserved-scope targets, capacity overflow, or input-shape
// failures return the error envelope.
//
// export_php:function scopecache_warm(array $grouped): ?array
func scopecache_warm(grouped *C.zend_array) unsafe.Pointer {
	gw := defaultSlot.Load()
	if gw == nil {
		return nil
	}
	goGrouped, err := phpArrayToGroupedItems(grouped)
	if err != nil {
		return errorEnvelopePtr(err.Error())
	}
	n, err := gw.Warm(goGrouped)
	if err != nil {
		return errorEnvelopePtr(errorMsg(err))
	}
	arr := phpAssocNew(2)
	phpAssocAddBool(arr, "ok", true)
	phpAssocAddLong(arr, "scopes", int64(n))
	return unsafe.Pointer(arr)
}

// scopecache_rebuild returns the POST /rebuild envelope as a PHP
// array. Atomically replaces the entire user-managed cache state
// with `grouped`; reserved scopes are re-created under the same
// all-shard write lock.
//
// Success:
//
//	['ok' => true, 'scopes' => N, 'items' => M]
//
// `scopes` and `items` reflect the post-rebuild totals (including
// the two reserved scopes the cache re-creates). Differs from
// scopecache_warm: rebuild drops anything not named in `grouped`,
// warm leaves untouched scopes alone.
//
// export_php:function scopecache_rebuild(array $grouped): ?array
func scopecache_rebuild(grouped *C.zend_array) unsafe.Pointer {
	gw := defaultSlot.Load()
	if gw == nil {
		return nil
	}
	goGrouped, err := phpArrayToGroupedItems(grouped)
	if err != nil {
		return errorEnvelopePtr(err.Error())
	}
	scopes, items, err := gw.Rebuild(goGrouped)
	if err != nil {
		return errorEnvelopePtr(errorMsg(err))
	}
	arr := phpAssocNew(3)
	phpAssocAddBool(arr, "ok", true)
	phpAssocAddLong(arr, "scopes", int64(scopes))
	phpAssocAddLong(arr, "items", int64(items))
	return unsafe.Pointer(arr)
}
