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
// to its HTTP-endpoint counterpart in RFC §6. The two `?string`
// variants (scopecache_render_by_*) return raw payload bytes, same
// as GET /render. Every other function returns `?array` — the PHP-
// array form of the HTTP success envelope, with payload decoded as
// `json_decode($body, true)` would decode it. Errors come back as
// `{ok:false, error:msg}`. A nil return crosses to PHP as null and
// means "no caddymodule loaded" (Provision never ran). The
// canonical wire shapes live in response_types.go in the core; the
// build* helpers below mirror them.
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
//     boundary. Payload bytes on reads are already Gateway-cloned,
//     so PHP owns them outright.

package scopecache_ext

/*
#include <php.h>

// Static-inline trampolines for cgo: PHP's ZVAL_* / zend_new_array
// are macros, so cgo cannot invoke them directly. The sc_* prefix
// makes the source of each symbol unambiguous at the call site.
// <php.h> pulls in zend_types.h, zend_string.h, zend_hash.h and
// everything else the API exposes.
static void sc_zval_str(zval *zv, zend_string *s) {
    ZVAL_STR(zv, s);
}
static void sc_zval_empty_string(zval *zv) {
    ZVAL_EMPTY_STRING(zv);
}
static void sc_zval_bool(zval *zv, int b) {
    ZVAL_BOOL(zv, b);
}
static void sc_zval_long(zval *zv, zend_long n) {
    ZVAL_LONG(zv, n);
}
static void sc_zval_double(zval *zv, double d) {
    ZVAL_DOUBLE(zv, d);
}
static void sc_zval_null(zval *zv) {
    ZVAL_NULL(zv);
}
static void sc_zval_arr(zval *zv, zend_array *a) {
    ZVAL_ARR(zv, a);
}
static zend_array *sc_zend_new_array(uint32_t size) {
    return zend_new_array(size);
}
// zend_hash_str_add_new — the no-collision variant of the insert
// macro; faster, and our build* helpers always use unique keys.
static zval *sc_hash_str_add(zend_array *ht, const char *key, size_t key_len, zval *zv) {
    return zend_hash_str_add_new(ht, key, key_len, zv);
}

// sc_add_* — fused zval-construct + hash-insert helpers, one cgo
// crossing per add (vs. the two-step ZVAL_* + hash_str_add the
// Go-side helpers used to pay).
static void sc_add_bool(zend_array *ht, const char *key, size_t key_len, int b) {
    zval zv; ZVAL_BOOL(&zv, b);
    zend_hash_str_add_new(ht, key, key_len, &zv);
}
static void sc_add_long(zend_array *ht, const char *key, size_t key_len, zend_long n) {
    zval zv; ZVAL_LONG(&zv, n);
    zend_hash_str_add_new(ht, key, key_len, &zv);
}
static void sc_add_double(zend_array *ht, const char *key, size_t key_len, double d) {
    zval zv; ZVAL_DOUBLE(&zv, d);
    zend_hash_str_add_new(ht, key, key_len, &zv);
}
static void sc_add_null(zend_array *ht, const char *key, size_t key_len) {
    zval zv; ZVAL_NULL(&zv);
    zend_hash_str_add_new(ht, key, key_len, &zv);
}
static void sc_add_string(zend_array *ht, const char *key, size_t key_len,
                                  const char *str_data, size_t str_len) {
    zval zv;
    if (str_len == 0) {
        ZVAL_EMPTY_STRING(&zv);
    } else {
        zend_string *zs = zend_string_init(str_data, str_len, 0);
        ZVAL_STR(&zv, zs);
    }
    zend_hash_str_add_new(ht, key, key_len, &zv);
}
static void sc_add_arr(zend_array *ht, const char *key, size_t key_len, zend_array *a) {
    zval zv; ZVAL_ARR(&zv, a);
    zend_hash_str_add_new(ht, key, key_len, &zv);
}

// sc_packed_push_arr — append a zend_array as the next packed-index
// element of an outer packed array. Collapses the two-cgo pattern
// (ZVAL_ARR + zend_hash_next_index_insert) into one call.
static void sc_packed_push_arr(zend_array *outer, zend_array *inner) {
    zval zv; ZVAL_ARR(&zv, inner);
    zend_hash_next_index_insert(outer, &zv);
}

// sc_zval_str_from_bytes — zend_string_init + ZVAL_STR in one cgo call.
static void sc_zval_str_from_bytes(zval *zv, const char *data, size_t len) {
    if (len == 0) {
        ZVAL_EMPTY_STRING(zv);
    } else {
        zend_string *zs = zend_string_init(data, len, 0);
        ZVAL_STR(zv, zs);
    }
}

// sc_packed_push_str_from_bytes — fresh zend_string + packed-index
// insert in one cgo call. Used by the JSON-array fast path.
static void sc_packed_push_str_from_bytes(zend_array *outer, const char *data, size_t len) {
    zval zv;
    if (len == 0) {
        ZVAL_EMPTY_STRING(&zv);
    } else {
        zend_string *zs = zend_string_init(data, len, 0);
        ZVAL_STR(&zv, zs);
    }
    zend_hash_next_index_insert(outer, &zv);
}

// sc_packed_push_long / _push_double / _push_bool / _push_null —
// scalar packed-array push in one cgo call.
static void sc_packed_push_long(zend_array *outer, zend_long n) {
    zval zv; ZVAL_LONG(&zv, n);
    zend_hash_next_index_insert(outer, &zv);
}
static void sc_packed_push_double(zend_array *outer, double d) {
    zval zv; ZVAL_DOUBLE(&zv, d);
    zend_hash_next_index_insert(outer, &zv);
}
static void sc_packed_push_bool(zend_array *outer, int b) {
    zval zv; ZVAL_BOOL(&zv, b);
    zend_hash_next_index_insert(outer, &zv);
}
static void sc_packed_push_null(zend_array *outer) {
    zval zv; ZVAL_NULL(&zv);
    zend_hash_next_index_insert(outer, &zv);
}

// sc_assoc_add_str_from_bytes — string-keyed counterpart of
// sc_packed_push_str_from_bytes. Used by the JSON-object fast path.
static void sc_assoc_add_str_from_bytes(zend_array *arr,
        const char *key, size_t key_len,
        const char *data, size_t data_len) {
    zval zv;
    if (data_len == 0) {
        ZVAL_EMPTY_STRING(&zv);
    } else {
        zend_string *zs = zend_string_init(data, data_len, 0);
        ZVAL_STR(&zv, zs);
    }
    zend_hash_str_add_new(arr, key, key_len, &zv);
}

// sc_build_item_array assembles the 5-key per-item PHP-array
// (scope, id|null, seq, ts, payload-or-event) in one cgo call.
// Caller pre-decodes the payload zval; reserved-scope rename to
// "event" is handled inline.
static zend_array *sc_build_item_array(
    const char *scope_data, size_t scope_len,
    const char *id_data,    size_t id_len,    int id_is_null,
    zend_long seq, zend_long ts,
    zval *payload_zv,
    int is_events_scope
) {
    zend_array *arr = zend_new_array(5);
    zval zv;

    // scope (always set)
    zend_string *scope_str = zend_string_init(scope_data, scope_len, 0);
    ZVAL_STR(&zv, scope_str);
    zend_hash_str_add_new(arr, "scope", sizeof("scope") - 1, &zv);

    // id (null for seq-only items; string otherwise)
    if (id_is_null) {
        ZVAL_NULL(&zv);
    } else {
        zend_string *id_str = zend_string_init(id_data, id_len, 0);
        ZVAL_STR(&zv, id_str);
    }
    zend_hash_str_add_new(arr, "id", sizeof("id") - 1, &zv);

    ZVAL_LONG(&zv, seq);
    zend_hash_str_add_new(arr, "seq", sizeof("seq") - 1, &zv);

    ZVAL_LONG(&zv, ts);
    zend_hash_str_add_new(arr, "ts", sizeof("ts") - 1, &zv);

    // payload / event — payload_zv is consumed by zend_hash_str_add_new
    // (it copies the zval contents and takes ownership of the refcount).
    if (is_events_scope) {
        zend_hash_str_add_new(arr, "event", sizeof("event") - 1, payload_zv);
    } else {
        zend_hash_str_add_new(arr, "payload", sizeof("payload") - 1, payload_zv);
    }

    return arr;
}

// sc_build_write_ack_array — same one-shot pattern for /append and
// /upsert response items (no payload, no event renaming). The same
// id/null switch applies.
static zend_array *sc_build_write_ack_array(
    const char *scope_data, size_t scope_len,
    const char *id_data,    size_t id_len,    int id_is_null,
    zend_long seq, zend_long ts
) {
    zend_array *arr = zend_new_array(4);
    zval zv;

    zend_string *scope_str = zend_string_init(scope_data, scope_len, 0);
    ZVAL_STR(&zv, scope_str);
    zend_hash_str_add_new(arr, "scope", sizeof("scope") - 1, &zv);

    if (id_is_null) {
        ZVAL_NULL(&zv);
    } else {
        zend_string *id_str = zend_string_init(id_data, id_len, 0);
        ZVAL_STR(&zv, id_str);
    }
    zend_hash_str_add_new(arr, "id", sizeof("id") - 1, &zv);

    ZVAL_LONG(&zv, seq);
    zend_hash_str_add_new(arr, "seq", sizeof("seq") - 1, &zv);

    ZVAL_LONG(&zv, ts);
    zend_hash_str_add_new(arr, "ts", sizeof("ts") - 1, &zv);

    return arr;
}
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"strconv"
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
// hand to PHP. Empty input returns nil, which the build-time
// wrapper patch (RETURN_EMPTY_STRING→RETURN_NULL) surfaces as PHP
// null — fine because validatePayload rejects empty payloads upstream.
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
// Build the response envelopes directly into PHP HashTables via the
// cgo trampolines above — no JSON-in-Go-then-decode-in-PHP detour.
// Source-of-truth for the wire shapes is response_types.go in the
// core. phpAssocAdd* each do one cgo crossing per add; key bytes
// are copied by zend_hash_str_add_new before the call returns, so
// the Go-string key need not outlive the cgo crossing.

func phpAssocNew(size int) *C.zend_array {
	return C.sc_zend_new_array(C.uint32_t(size))
}

// keyPtr returns the (char*, size_t) pair for a Go string key.
func keyPtr(key string) (*C.char, C.size_t) {
	if len(key) == 0 {
		return nil, 0
	}
	return (*C.char)(unsafe.Pointer(unsafe.StringData(key))), C.size_t(len(key))
}

func phpAssocAddBool(arr *C.zend_array, key string, val bool) {
	kp, kl := keyPtr(key)
	var b C.int
	if val {
		b = 1
	}
	C.sc_add_bool(arr, kp, kl, b)
}

func phpAssocAddLong(arr *C.zend_array, key string, val int64) {
	kp, kl := keyPtr(key)
	C.sc_add_long(arr, kp, kl, C.zend_long(val))
}

func phpAssocAddDouble(arr *C.zend_array, key string, val float64) {
	kp, kl := keyPtr(key)
	C.sc_add_double(arr, kp, kl, C.double(val))
}

func phpAssocAddNull(arr *C.zend_array, key string) {
	kp, kl := keyPtr(key)
	C.sc_add_null(arr, kp, kl)
}

func phpAssocAddString(arr *C.zend_array, key string, val string) {
	kp, kl := keyPtr(key)
	var sp *C.char
	if len(val) > 0 {
		sp = (*C.char)(unsafe.Pointer(unsafe.StringData(val)))
	}
	C.sc_add_string(arr, kp, kl, sp, C.size_t(len(val)))
}

func phpAssocAddArray(arr *C.zend_array, key string, val *C.zend_array) {
	kp, kl := keyPtr(key)
	C.sc_add_arr(arr, kp, kl, val)
}

func phpAssocAddZval(arr *C.zend_array, key string, zv *C.zval) {
	kp, kl := keyPtr(key)
	C.sc_hash_str_add(arr, kp, kl, zv)
}

// payloadToZval decodes a stored item's JSON payload bytes and writes
// the resulting PHP value into zv. Hand-rolled byte-walker — bypasses
// json.Decoder entirely so we avoid per-call Decoder/Reader allocation
// and the interface{}-boxed Token() return type.
//
// Correctness contract:
//   - Object key order is preserved (we iterate source bytes in order).
//   - Numbers without `.` / `e` / `E` become PHP int via ZVAL_LONG;
//     others become PHP float via ZVAL_DOUBLE. Matches `json_decode`.
//   - Strings without backslash escapes take the zero-copy fast path
//     (alias the source bytes into a fresh zend_string in one cgo).
//     Strings with escapes fall through to encoding/json for the
//     escape-decoding step — slower but still correct.
//
// Empty input (defensive — validatePayload rejects this upstream)
// writes PHP null.
func payloadToZval(payload json.RawMessage, zv *C.zval) {
	if len(payload) == 0 {
		C.sc_zval_null(zv)
		return
	}
	p := jsonParser{b: payload}
	if err := p.parseValue(zv); err != nil {
		C.sc_zval_null(zv)
	}
}

type jsonParser struct {
	b   []byte
	pos int
}

func (p *jsonParser) skipWS() {
	for p.pos < len(p.b) {
		switch p.b[p.pos] {
		case ' ', '\t', '\n', '\r':
			p.pos++
		default:
			return
		}
	}
}

func (p *jsonParser) parseValue(zv *C.zval) error {
	p.skipWS()
	if p.pos >= len(p.b) {
		return fmt.Errorf("payload decode: unexpected EOF")
	}
	c := p.b[p.pos]
	switch {
	case c == '{':
		return p.parseObject(zv)
	case c == '[':
		return p.parseArray(zv)
	case c == '"':
		return p.parseStringValue(zv)
	case c == 't':
		return p.parseLiteral([]byte("true"), zv, parseLiteralTrue)
	case c == 'f':
		return p.parseLiteral([]byte("false"), zv, parseLiteralFalse)
	case c == 'n':
		return p.parseLiteral([]byte("null"), zv, parseLiteralNull)
	case c == '-' || (c >= '0' && c <= '9'):
		return p.parseNumber(zv)
	default:
		return fmt.Errorf("payload decode: unexpected byte %q at pos %d", c, p.pos)
	}
}

const (
	parseLiteralTrue  = 1
	parseLiteralFalse = 2
	parseLiteralNull  = 3
)

func (p *jsonParser) parseLiteral(want []byte, zv *C.zval, kind int) error {
	if p.pos+len(want) > len(p.b) {
		return fmt.Errorf("payload decode: truncated literal at pos %d", p.pos)
	}
	for i, b := range want {
		if p.b[p.pos+i] != b {
			return fmt.Errorf("payload decode: bad literal at pos %d", p.pos)
		}
	}
	p.pos += len(want)
	switch kind {
	case parseLiteralTrue:
		C.sc_zval_bool(zv, 1)
	case parseLiteralFalse:
		C.sc_zval_bool(zv, 0)
	default:
		C.sc_zval_null(zv)
	}
	return nil
}

func (p *jsonParser) parseNumber(zv *C.zval) error {
	start := p.pos
	if p.b[p.pos] == '-' {
		p.pos++
	}
	for p.pos < len(p.b) && p.b[p.pos] >= '0' && p.b[p.pos] <= '9' {
		p.pos++
	}
	isFloat := false
	if p.pos < len(p.b) && p.b[p.pos] == '.' {
		isFloat = true
		p.pos++
		for p.pos < len(p.b) && p.b[p.pos] >= '0' && p.b[p.pos] <= '9' {
			p.pos++
		}
	}
	if p.pos < len(p.b) && (p.b[p.pos] == 'e' || p.b[p.pos] == 'E') {
		isFloat = true
		p.pos++
		if p.pos < len(p.b) && (p.b[p.pos] == '+' || p.b[p.pos] == '-') {
			p.pos++
		}
		for p.pos < len(p.b) && p.b[p.pos] >= '0' && p.b[p.pos] <= '9' {
			p.pos++
		}
	}
	s := unsafe.String(unsafe.SliceData(p.b[start:p.pos]), p.pos-start)
	if !isFloat {
		if i, err := strconv.ParseInt(s, 10, 64); err == nil {
			C.sc_zval_long(zv, C.zend_long(i))
			return nil
		}
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return fmt.Errorf("payload decode: bad number %q", s)
	}
	C.sc_zval_double(zv, C.double(f))
	return nil
}

// parseRawString locates the start and end of a JSON string token,
// returning the byte slice between the quotes plus a flag indicating
// whether the slice contains any backslash-escape sequences. On the
// no-escape fast path the caller can use the raw bytes directly; on
// the slow path encoding/json.Unmarshal handles escape decoding.
//
// p.pos must point at the opening `"`. On return, p.pos is one past
// the closing `"`.
func (p *jsonParser) parseRawString() (rawStart, rawEnd int, hasEscape bool, err error) {
	if p.pos >= len(p.b) || p.b[p.pos] != '"' {
		return 0, 0, false, fmt.Errorf("payload decode: expected string at pos %d", p.pos)
	}
	p.pos++ // past opening quote
	rawStart = p.pos
	for p.pos < len(p.b) {
		c := p.b[p.pos]
		switch c {
		case '"':
			rawEnd = p.pos
			p.pos++ // past closing quote
			return rawStart, rawEnd, hasEscape, nil
		case '\\':
			hasEscape = true
			p.pos++
			if p.pos < len(p.b) {
				p.pos++ // skip the escaped byte
			}
		default:
			p.pos++
		}
	}
	return 0, 0, false, fmt.Errorf("payload decode: unterminated string")
}

func (p *jsonParser) parseStringValue(zv *C.zval) error {
	start, end, hasEscape, err := p.parseRawString()
	if err != nil {
		return err
	}
	if !hasEscape {
		if end == start {
			C.sc_zval_empty_string(zv)
			return nil
		}
		C.sc_zval_str_from_bytes(
			zv,
			(*C.char)(unsafe.Pointer(&p.b[start])),
			C.size_t(end-start),
		)
		return nil
	}
	// Slow path: re-decode through encoding/json so escape semantics
	// (\n, \t, \uXXXX, etc.) stay identical to PHP json_decode.
	var s string
	if err := json.Unmarshal(p.b[start-1:end+1], &s); err != nil {
		return err
	}
	if len(s) == 0 {
		C.sc_zval_empty_string(zv)
		return nil
	}
	C.sc_zval_str_from_bytes(
		zv,
		(*C.char)(unsafe.Pointer(unsafe.StringData(s))),
		C.size_t(len(s)),
	)
	return nil
}

func (p *jsonParser) parseObject(zv *C.zval) error {
	p.pos++ // skip '{'
	arr := C.sc_zend_new_array(8)
	p.skipWS()
	if p.pos < len(p.b) && p.b[p.pos] == '}' {
		p.pos++
		C.sc_zval_arr(zv, arr)
		return nil
	}
	for {
		p.skipWS()
		// Key — always a JSON string.
		ks, ke, kEsc, err := p.parseRawString()
		if err != nil {
			return err
		}
		var keyData *C.char
		var keyLen C.size_t
		var keyHolder string // alive across the cgo call when escape path is hit
		if !kEsc {
			if ke > ks {
				keyData = (*C.char)(unsafe.Pointer(&p.b[ks]))
			}
			keyLen = C.size_t(ke - ks)
		} else {
			if err := json.Unmarshal(p.b[ks-1:ke+1], &keyHolder); err != nil {
				return err
			}
			if len(keyHolder) > 0 {
				keyData = (*C.char)(unsafe.Pointer(unsafe.StringData(keyHolder)))
			}
			keyLen = C.size_t(len(keyHolder))
		}
		p.skipWS()
		if p.pos >= len(p.b) || p.b[p.pos] != ':' {
			return fmt.Errorf("payload decode: expected ':' at pos %d", p.pos)
		}
		p.pos++
		// Value-specific fast paths inline the (parseValue + insert)
		// pair into one cgo call where the value type is trivial.
		p.skipWS()
		if p.pos >= len(p.b) {
			return fmt.Errorf("payload decode: unexpected EOF")
		}
		if p.b[p.pos] == '"' {
			vs, ve, vEsc, err := p.parseRawString()
			if err != nil {
				return err
			}
			if !vEsc {
				var vp *C.char
				if ve > vs {
					vp = (*C.char)(unsafe.Pointer(&p.b[vs]))
				}
				C.sc_assoc_add_str_from_bytes(arr, keyData, keyLen, vp, C.size_t(ve-vs))
			} else {
				var s string
				if err := json.Unmarshal(p.b[vs-1:ve+1], &s); err != nil {
					return err
				}
				var sp *C.char
				if len(s) > 0 {
					sp = (*C.char)(unsafe.Pointer(unsafe.StringData(s)))
				}
				C.sc_assoc_add_str_from_bytes(arr, keyData, keyLen, sp, C.size_t(len(s)))
			}
		} else {
			var valZv C.zval
			if err := p.parseValue(&valZv); err != nil {
				return err
			}
			C.sc_hash_str_add(arr, keyData, keyLen, &valZv)
		}
		_ = keyHolder // keep alive for cgo call duration
		p.skipWS()
		if p.pos < len(p.b) {
			switch p.b[p.pos] {
			case ',':
				p.pos++
				continue
			case '}':
				p.pos++
				C.sc_zval_arr(zv, arr)
				return nil
			}
		}
		return fmt.Errorf("payload decode: expected ',' or '}' at pos %d", p.pos)
	}
}

func (p *jsonParser) parseArray(zv *C.zval) error {
	p.pos++ // skip '['
	arr := C.sc_zend_new_array(8)
	p.skipWS()
	if p.pos < len(p.b) && p.b[p.pos] == ']' {
		p.pos++
		C.sc_zval_arr(zv, arr)
		return nil
	}
	for {
		p.skipWS()
		if p.pos >= len(p.b) {
			return fmt.Errorf("payload decode: unexpected EOF")
		}
		// Element-specific fast paths same as parseObject.
		if p.b[p.pos] == '"' {
			vs, ve, vEsc, err := p.parseRawString()
			if err != nil {
				return err
			}
			if !vEsc {
				var vp *C.char
				if ve > vs {
					vp = (*C.char)(unsafe.Pointer(&p.b[vs]))
				}
				C.sc_packed_push_str_from_bytes(arr, vp, C.size_t(ve-vs))
			} else {
				var s string
				if err := json.Unmarshal(p.b[vs-1:ve+1], &s); err != nil {
					return err
				}
				var sp *C.char
				if len(s) > 0 {
					sp = (*C.char)(unsafe.Pointer(unsafe.StringData(s)))
				}
				C.sc_packed_push_str_from_bytes(arr, sp, C.size_t(len(s)))
			}
		} else {
			var valZv C.zval
			if err := p.parseValue(&valZv); err != nil {
				return err
			}
			C.zend_hash_next_index_insert(arr, &valZv)
		}
		p.skipWS()
		if p.pos < len(p.b) {
			switch p.b[p.pos] {
			case ',':
				p.pos++
				continue
			case ']':
				p.pos++
				C.sc_zval_arr(zv, arr)
				return nil
			}
		}
		return fmt.Errorf("payload decode: expected ',' or ']' at pos %d", p.pos)
	}
}

// buildItemAssoc emits the per-item PHP-array used inside `item` on
// /get and as each element of `items` on /head + /tail. One cgo
// crossing via sc_build_item_array; payload is decoded Go-side first.
func buildItemAssoc(item scopecache.Item) *C.zend_array {
	var payloadZv C.zval
	payloadToZval(item.Payload, &payloadZv)

	var (
		scopeData *C.char
		idData    *C.char
		idLen     C.size_t
		idIsNull  C.int = 1
	)
	if len(item.Scope) > 0 {
		scopeData = (*C.char)(unsafe.Pointer(unsafe.StringData(item.Scope)))
	}
	if item.ID != "" {
		idData = (*C.char)(unsafe.Pointer(unsafe.StringData(item.ID)))
		idLen = C.size_t(len(item.ID))
		idIsNull = 0
	}
	var isEvents C.int
	if item.Scope == scopecache.EventsScopeName {
		isEvents = 1
	}
	return C.sc_build_item_array(
		scopeData, C.size_t(len(item.Scope)),
		idData, idLen, idIsNull,
		C.zend_long(item.Seq), C.zend_long(item.Ts),
		&payloadZv,
		isEvents,
	)
}

// buildWriteAckAssoc emits the `item` sub-array for /append and
// /upsert responses (no payload — client just supplied it).
func buildWriteAckAssoc(item scopecache.Item) *C.zend_array {
	var (
		scopeData *C.char
		idData    *C.char
		idLen     C.size_t
		idIsNull  C.int = 1
	)
	if len(item.Scope) > 0 {
		scopeData = (*C.char)(unsafe.Pointer(unsafe.StringData(item.Scope)))
	}
	if item.ID != "" {
		idData = (*C.char)(unsafe.Pointer(unsafe.StringData(item.ID)))
		idLen = C.size_t(len(item.ID))
		idIsNull = 0
	}
	return C.sc_build_write_ack_array(
		scopeData, C.size_t(len(item.Scope)),
		idData, idLen, idIsNull,
		C.zend_long(item.Seq), C.zend_long(item.Ts),
	)
}

// buildItemsPackedArray emits the inner `items` packed-array of
// /head + /tail responses; one buildItemAssoc per element.
func buildItemsPackedArray(items []scopecache.Item) *C.zend_array {
	arr := C.sc_zend_new_array(C.uint32_t(len(items)))
	for i := range items {
		C.sc_packed_push_arr(arr, buildItemAssoc(items[i]))
	}
	return arr
}

// buildItemsEnvelope assembles the /head + /tail success envelope.
// withOffset toggles the /tail-only `offset` field.
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

// buildReservedScopesArray emits /stats's reserved_scopes block —
// six-field slim row, no last_access_ts / read_count_total.
func buildReservedScopesArray(rows []scopecache.ReservedScopeEntry) *C.zend_array {
	arr := C.sc_zend_new_array(C.uint32_t(len(rows)))
	for i := range rows {
		row := phpAssocNew(6)
		phpAssocAddString(row, "scope", rows[i].Scope)
		phpAssocAddLong(row, "item_count", int64(rows[i].ItemCount))
		phpAssocAddLong(row, "last_seq", int64(rows[i].LastSeq))
		phpAssocAddDouble(row, "approx_scope_mb", float64(rows[i].ApproxScopeMB)/1048576.0)
		phpAssocAddLong(row, "created_ts", rows[i].CreatedTS)
		phpAssocAddLong(row, "last_write_ts", rows[i].LastWriteTS)
		C.sc_packed_push_arr(arr, row)
	}
	return arr
}

// buildScopeListPackedArray emits /scopelist's `scopes` field —
// full eight-field row per scope (including read-bookkeeping).
func buildScopeListPackedArray(rows []scopecache.ScopeListEntry) *C.zend_array {
	arr := C.sc_zend_new_array(C.uint32_t(len(rows)))
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
		C.sc_packed_push_arr(arr, row)
	}
	return arr
}

// buildHitCountEnvelope returns the {ok, hit, count} shape shared by
// /delete and /delete_up_to; hit = count > 0.
func buildHitCountEnvelope(count int) *C.zend_array {
	arr := phpAssocNew(3)
	phpAssocAddBool(arr, "ok", true)
	phpAssocAddBool(arr, "hit", count > 0)
	phpAssocAddLong(arr, "count", int64(count))
	return arr
}

// buildErrorEnvelope returns the {ok:false, error:msg} shape.
func buildErrorEnvelope(msg string) *C.zend_array {
	arr := phpAssocNew(2)
	phpAssocAddBool(arr, "ok", false)
	phpAssocAddString(arr, "error", msg)
	return arr
}

// errorEnvelopePtr is the unsafe.Pointer form of buildErrorEnvelope.
func errorEnvelopePtr(msg string) unsafe.Pointer {
	return unsafe.Pointer(buildErrorEnvelope(msg))
}

// errorMsg renders a Gateway error as the string that goes on the
// PHP-side error envelope. Mirrors the HTTP error-body text.
func errorMsg(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// scopecache_get — GET /get envelope as a PHP array.
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

// scopecache_tail — GET /tail envelope as a PHP array. `offset` in
// the envelope is always 0; the PHP signature does not expose paging.
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

// scopecache_head — GET /head envelope as a PHP array. `after_seq`
// is the forward-cursor (0 starts at the beginning).
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

// scopecache_get_by_seq — GET /get envelope, addressed by seq.
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

// scopecache_render_by_id — GET /render raw bytes (no envelope).
// JSON-string payloads are emitted unwrapped; other shapes pass
// through as canonical JSON.
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

// scopecache_render_by_seq — GET /render raw bytes, addressed by seq.
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

// scopecache_append — POST /append envelope. `created` is always
// true on this endpoint; carried for write-envelope uniformity.
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

// scopecache_upsert — POST /upsert envelope. `created` distinguishes
// first-write from in-place replace; seq is preserved on replace.
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

// scopecache_update — POST /update envelope. `created` is always
// false on this endpoint; `count` is 0 on miss, 1 on hit.
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

// scopecache_counter_add — POST /counter_add envelope. `created`
// is true on first-touch; `value` is the post-add counter value.
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

// scopecache_delete — POST /delete envelope. `count` is 0 or 1.
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

// scopecache_delete_by_seq — POST /delete envelope, addressed by seq.
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

// scopecache_delete_up_to — POST /delete_up_to envelope. `count`
// is the number of items released.
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

// scopecache_delete_scope — POST /delete_scope envelope. `hit`
// reflects "scope existed pre-call" (empty-but-existing still hits).
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

// scopecache_wipe — POST /wipe envelope. `scopes` / `items` count
// what was dropped (reserved scopes are dropped + immediately
// re-created, so a fresh-booted wipe still reports scopes=2).
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

// scopecache_stats — GET /stats envelope as a PHP array.
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

// scopecache_scopelist — GET /scopelist envelope. `prefix` "" = no
// filter; `after` "" = start from beginning; `limit` clamped server-side.
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

// scopecache_warm — POST /warm envelope. Replaces every scope
// present in `grouped`; scopes not in `grouped` are untouched.
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

// scopecache_rebuild — POST /rebuild envelope. Atomically replaces
// the entire user-managed cache state with `grouped`; reserved
// scopes are re-created under the same all-shard write lock.
// Unlike /warm, scopes not in `grouped` are dropped.
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
