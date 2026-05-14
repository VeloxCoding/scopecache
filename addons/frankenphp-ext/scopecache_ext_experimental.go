// Experimental cgo entry-points. NOT part of the production extension —
// these exist to A/B benchmark alternative implementations against the
// shipping `scopecache_*` functions.
//
// Boundaries:
//
//   - This file is EXCLUDED from production builds (tools/frankenphp-bin
//     deletes it before invoking frankenphp-gen, so no wrappers get
//     generated and no symbols leak into the static binary).
//   - The bench/dynamic build (tools/frankenphp-ext) DOES include it,
//     so bench-experimental.{sh,php} can call these functions.
//
// Conventions:
//
//   - Every function name starts with `scopecache_x_` (the `_x_` tag
//     signals "experimental" both in the PHP-side call site and in
//     grep-ability).
//   - Each function carries a doc-comment explaining what variation
//     it tests (constant payload, payload-only, hand-rolled JSON,
//     etc.) so a reader doesn't have to diff against scopecache_ext.go.
//   - When an experiment is settled (won or lost), promote / delete
//     here, do not let dead variants accumulate.

package scopecache_ext

/*
#include <php.h>
*/
import "C"

import (
	"encoding/json"
	"strconv"
	"unsafe"

	"github.com/VeloxCoding/scopecache"
)

// --- BEGIN MERGE-INTO-MAIN-EXT ---
// (Everything below this marker gets appended to scopecache_ext.go by
// tools/frankenphp-ext/build.sh before frankenphp-gen runs, because
// frankenphp-gen extension-init only processes one file. The marker
// is stable and grep-able.)

// --- baseline measurements ----------------------------------------

// scopecache_x_constant_json — fixed string, no gateway call, no
// json.Marshal. Measures pure cgo cost (string-out + boundary).
// Subtract this from any other JSON-string variant to isolate the
// non-cgo work.
//
// export_php:function scopecache_x_constant_json(): ?string
func scopecache_x_constant_json() unsafe.Pointer {
	bytes := []byte(`{"ok":true,"hit":true,"count":1,"item":{"scope":"x","id":"x","seq":1,"ts":0,"payload":{"v":1}}}`)
	return phpStringFromBytes(bytes)
}

// scopecache_x_payload_only — gateway lookup + raw payload bytes
// as a PHP string. No envelope, no metadata. Mirrors the historical
// pre-Phase-A shape of scopecache_get (~0.3 µs on previous hosts).
// Measures lookup + payload-copy + cgo (no json.Marshal at all).
//
// export_php:function scopecache_x_payload_only(string $scope, string $id): ?string
func scopecache_x_payload_only(scope *C.zend_string, id *C.zend_string) unsafe.Pointer {
	gw := defaultSlot.Load()
	if gw == nil {
		return nil
	}
	item, found := gw.GetByID(zendStringView(scope), zendStringView(id))
	if !found {
		return nil
	}
	return phpStringFromBytes(item.Payload)
}

// scopecache_x_get_json — full envelope as JSON string via json.Marshal.
// Same shape as scopecache_get's PHP-array return, but built through
// Go's stdlib JSON encoder instead of cgo HashTable adds. Subtract
// scopecache_x_payload_only from this to isolate the json.Marshal +
// envelope-wrap cost.
//
// export_php:function scopecache_x_get_json(string $scope, string $id): ?string
func scopecache_x_get_json(scope *C.zend_string, id *C.zend_string) unsafe.Pointer {
	gw := defaultSlot.Load()
	if gw == nil {
		return nil
	}
	item, found := gw.GetByID(zendStringView(scope), zendStringView(id))
	env := scopecache.GetResponse{OK: true, Hit: found}
	if found {
		env.Count = 1
		env.Item = &item
	}
	bytes, _ := json.Marshal(env)
	return phpStringFromBytes(bytes)
}

// scopecache_x_marshal_only — feeds a pre-built Item through
// json.Marshal and discards the result. NO gateway call, NO cgo
// crossing into PHP. Measures json.Marshal in isolation.
//
// export_php:function scopecache_x_marshal_only(): ?string
func scopecache_x_marshal_only() unsafe.Pointer {
	item := scopecache.Item{
		Scope:   "x",
		ID:      "x",
		Seq:     1,
		Ts:      0,
		Payload: json.RawMessage(`{"v":1}`),
	}
	env := scopecache.GetResponse{OK: true, Hit: true, Count: 1, Item: &item}
	bytes, _ := json.Marshal(env)
	return phpStringFromBytes(bytes)
}

// scopecache_x_marshal_item_only — json.Marshal of a single Item,
// no envelope. Isolates Item.MarshalJSON cost (anonymous struct alloc
// + reflection over 5 fields, the bulk of which we suspect dominates
// the full json.Marshal of GetResponse).
//
// export_php:function scopecache_x_marshal_item_only(): ?string
func scopecache_x_marshal_item_only() unsafe.Pointer {
	item := scopecache.Item{
		Scope:   "x",
		ID:      "x",
		Seq:     1,
		Ts:      0,
		Payload: json.RawMessage(`{"v":1}`),
	}
	bytes, _ := json.Marshal(item)
	return phpStringFromBytes(bytes)
}

// scopecache_x_marshal_no_item — json.Marshal of a GetResponse with
// Item=nil. Measures the envelope-wrap cost (ok/hit/count fields)
// without invoking Item.MarshalJSON. Subtract this from marshal_only
// to isolate Item.MarshalJSON specifically.
//
// export_php:function scopecache_x_marshal_no_item(): ?string
func scopecache_x_marshal_no_item() unsafe.Pointer {
	env := scopecache.GetResponse{OK: true, Hit: false, Count: 0, Item: nil}
	bytes, _ := json.Marshal(env)
	return phpStringFromBytes(bytes)
}

// --- hand-rolled JSON build (the alternative we want to A/B test) -

// appendItemJSONInline mirrors handlers_read.go's appendItemJSON
// byte-for-byte. Copied here because the upstream is unexported
// (lowercase a). If this experiment wins, promote the original
// to AppendItemJSON or add a new public Gateway method that
// returns the bytes; this duplicate goes away.
func appendItemJSONInline(buf []byte, item scopecache.Item) []byte {
	payloadKey := "payload"
	if item.Scope == scopecache.EventsScopeName {
		payloadKey = "event"
	}
	buf = append(buf, `{"scope":`...)
	buf = appendJSONStringInline(buf, item.Scope)
	buf = append(buf, `,"id":`...)
	if item.ID == "" {
		buf = append(buf, `null`...)
	} else {
		buf = appendJSONStringInline(buf, item.ID)
	}
	buf = append(buf, `,"seq":`...)
	buf = strconv.AppendUint(buf, item.Seq, 10)
	buf = append(buf, `,"ts":`...)
	buf = strconv.AppendInt(buf, item.Ts, 10)
	buf = append(buf, ',', '"')
	buf = append(buf, payloadKey...)
	buf = append(buf, '"', ':')
	if len(item.Payload) == 0 {
		buf = append(buf, `null`...)
	} else {
		buf = append(buf, item.Payload...)
	}
	return append(buf, '}')
}

// appendJSONStringInline mirrors handlers_read.go's appendJSONString
// fast path: linear scan for JSON-specials, inline-quote on the common
// case. Scope/id constrained by validation so the slow fallback is
// unreachable here — but we still call into encoding/json on miss,
// matching upstream semantics byte-for-byte.
func appendJSONStringInline(dst []byte, s string) []byte {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c == '"' || c == '\\' || c == '<' || c == '>' || c == '&' {
			b, _ := json.Marshal(s)
			return append(dst, b...)
		}
	}
	dst = append(dst, '"')
	dst = append(dst, s...)
	return append(dst, '"')
}

// scopecache_x_get_json_handrolled — full GetResponse envelope as a
// JSON string, built via hand-rolled buffer append (no json.Marshal,
// no reflection). Returns the SAME wire shape as scopecache_get's
// HTTP /get response: `{"ok":true,"hit":true,"count":1,"item":{...}}`.
// approx_response_mb is omitted here (it is server-side observability,
// not interesting for the cgo path).
//
// export_php:function scopecache_x_get_json_handrolled(string $scope, string $id): ?string
func scopecache_x_get_json_handrolled(scope *C.zend_string, id *C.zend_string) unsafe.Pointer {
	gw := defaultSlot.Load()
	if gw == nil {
		return nil
	}
	item, found := gw.GetByID(zendStringView(scope), zendStringView(id))
	buf := make([]byte, 0, 256)
	if !found {
		buf = append(buf, `{"ok":true,"hit":false,"count":0,"item":null}`...)
	} else {
		buf = append(buf, `{"ok":true,"hit":true,"count":1,"item":`...)
		buf = appendItemJSONInline(buf, item)
		buf = append(buf, '}')
	}
	return phpStringFromBytes(buf)
}
