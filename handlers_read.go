// Read handlers on the public mux:
//
//   - /head      — window after a seq cursor, oldest-first
//   - /tail      — window at the newest end, oldest-first within
//   - /get       — single-item lookup by id or seq, JSON envelope
//   - /render    — single-item raw payload, no JSON envelope
//
// /head and /tail share writeItemsHit / writeItemsMiss so the wire
// shape (`ok`, `hit`, `count`, `truncated`, `items`,
// `approx_response_mb`, `duration_us`) stays uniform across the
// list-returning read family. writeItemsHit enforces the
// per-response cap before marshal-and-write; the read-heat stamp
// (only on non-empty results) lives one layer down in Store.head /
// Store.tail.

package scopecache

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"
)

// writeItemsHit assembles and writes the success response for a
// list-returning read endpoint (/head, /tail). HTTP shape + per-
// response byte cap only — read-heat stamping lives in store.head /
// store.tail.
//
// extra slots between count and truncated so /tail can carry its
// offset field at the right wire position; /head passes nil. The
// fixed ok / hit / count / [extra] / truncated / items order is
// what clients and log scanners see — keep new fields in their
// correct slot rather than appending.
func (api *API) writeItemsHit(
	w http.ResponseWriter,
	started time.Time,
	items []Item,
	truncated bool,
	extra orderedFields,
) {
	if len(items) > 0 {
		if estimated := estimateMultiItemResponseBytes(items); estimated > api.maxResponseBytes {
			responseTooLarge(w, started, estimated, api.maxResponseBytes)
			return
		}
	}
	api.writeItemsResponse(w, started, items, truncated, extra)
}

// writeItemsMiss writes the canonical "scope does not exist" response
// for a list-returning read endpoint. Same field order as
// writeItemsHit's success path; truncated is always false; items is
// the sentinel empty slice (not nil — `[]Item{}` marshals as `[]`,
// nil would marshal as `null` and break clients that iterate). Goes
// through the same single-buffer writer as the hit path so cap
// behaviour stays symmetric across hit and miss responses.
func (api *API) writeItemsMiss(
	w http.ResponseWriter,
	started time.Time,
	extra orderedFields,
) {
	api.writeItemsResponse(w, started, nil, false, extra)
}

// writeItemsResponse builds the /head / /tail / /scopelist (when
// reused) envelope manually in a single buffer and writes it once.
// The path that this replaces (writeJSONWithMetaCap → marshalWith-
// ApproxSize) ran json.Marshal over the items array (one full
// payload-bytes copy) and then spliced duration_us+approx_response_mb
// into a fresh buffer (a second full copy). For 1000-item × 10 KiB
// /tail responses that meant ~64 MiB of allocation per request.
//
// The new path: one buffer, pre-grown from estimateMultiItemResponse-
// Bytes plus a small per-item JSON-skeleton headroom; per-item JSON
// emitted by appendItemJSON; raw payload bytes appended once into
// the buffer (still one copy per item — unavoidable without true
// streaming, which would need an upper-bound size estimator to keep
// the cap honest). Net savings: roughly half the previous allocation
// volume per request.
//
// Cap discipline:
//   - Pre-flight estimateMultiItemResponseBytes (LOWER bound) catches
//     pathologically large requests before any allocation; that check
//     lives in writeItemsHit.
//   - Post-flight check on the actual built buffer length is
//     authoritative; if exceeded, switch to a 507 envelope and
//     discard the built body.
func (api *API) writeItemsResponse(
	w http.ResponseWriter,
	started time.Time,
	items []Item,
	truncated bool,
	extra orderedFields,
) {
	// Pre-grow buf so the per-item appends don't trigger slice
	// re-grows on the hot path. The 32-byte per-item slack covers
	// the JSON skeleton plus seq/ts digits.
	estCapacity := int64(192) + int64(len(items))*32
	for i := range items {
		estCapacity += int64(len(items[i].Scope)) + int64(len(items[i].ID)) + int64(len(items[i].Payload))
	}
	buf := make([]byte, 0, estCapacity)

	// Envelope head: ok, hit, count
	buf = append(buf, `{"ok":true,"hit":`...)
	if len(items) > 0 {
		buf = append(buf, `true`...)
	} else {
		buf = append(buf, `false`...)
	}
	buf = append(buf, `,"count":`...)
	buf = strconv.AppendInt(buf, int64(len(items)), 10)

	// Extras (currently only /tail's "offset" int).
	for _, kv := range extra {
		buf = append(buf, ',', '"')
		buf = append(buf, kv.K...)
		buf = append(buf, '"', ':')
		buf = appendKVValue(buf, kv.V)
	}

	// truncated, items
	buf = append(buf, `,"truncated":`...)
	if truncated {
		buf = append(buf, `true`...)
	} else {
		buf = append(buf, `false`...)
	}
	buf = append(buf, `,"items":[`...)
	for i := range items {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = appendItemJSON(buf, items[i])
	}
	buf = append(buf, ']')

	// Suffix: duration_us + approx_response_mb (single-pass estimate).
	buf = append(buf, `,"duration_us":`...)
	buf = strconv.AppendInt(buf, time.Since(started).Microseconds(), 10)
	estTotal := len(buf) + 30
	mbVal := float64(estTotal) / 1048576.0
	buf = append(buf, `,"approx_response_mb":`...)
	buf = strconv.AppendFloat(buf, mbVal, 'f', 4, 64)
	buf = append(buf, '}', '\n')

	// Authoritative cap on the actual marshalled body.
	if int64(len(buf)) > api.maxResponseBytes {
		responseTooLarge(w, started, int64(len(buf)), api.maxResponseBytes)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf)
}

// appendItemJSON appends item to buf as JSON, mirroring
// Item.MarshalJSON byte-for-byte: scope/id/seq are dropped when
// zero-valued, ts and the payload-bearing field are always
// emitted, and items in the reserved _events scope rename
// "payload" to "event". Empty Payload (only reachable via
// white-box mutation; validatePayload blocks it on the write
// path) emits literal "null" instead of malformed `,"payload":}`.
func appendItemJSON(buf []byte, item Item) []byte {
	payloadKey := "payload"
	if item.Scope == EventsScopeName {
		payloadKey = "event"
	}

	buf = append(buf, '{')
	first := true
	if item.Scope != "" {
		buf = append(buf, `"scope":`...)
		buf = appendJSONString(buf, item.Scope)
		first = false
	}
	if item.ID != "" {
		if !first {
			buf = append(buf, ',')
		}
		buf = append(buf, `"id":`...)
		buf = appendJSONString(buf, item.ID)
		first = false
	}
	if item.Seq != 0 {
		if !first {
			buf = append(buf, ',')
		}
		buf = append(buf, `"seq":`...)
		buf = strconv.AppendUint(buf, item.Seq, 10)
		first = false
	}
	if !first {
		buf = append(buf, ',')
	}
	buf = append(buf, `"ts":`...)
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

// appendKVValue encodes a value as JSON into buf, fast-path for the
// scalar types orderedFields carries across read + write envelopes.
// Coverage: nil/bool/int/int64/uint64/string/MB plus the two
// in-package structs that appear nested inside response envelopes
// (writeAck for /append + /upsert, []ScopeCapacityOffender for the
// 507 capacity errors). Any other type falls through to json.Marshal
// so the wire format stays byte-for-byte identical to the previous
// path even if a future caller passes something exotic.
func appendKVValue(buf []byte, v interface{}) []byte {
	switch t := v.(type) {
	case nil:
		return append(buf, `null`...)
	case bool:
		if t {
			return append(buf, `true`...)
		}
		return append(buf, `false`...)
	case int:
		return strconv.AppendInt(buf, int64(t), 10)
	case int64:
		return strconv.AppendInt(buf, t, 10)
	case uint64:
		return strconv.AppendUint(buf, t, 10)
	case string:
		return appendJSONString(buf, t)
	case MB:
		// Matches MB.MarshalJSON's fmt.Sprintf("%.4f", v/1048576) byte-for-byte.
		return strconv.AppendFloat(buf, float64(t)/1048576.0, 'f', 4, 64)
	case writeAck:
		return appendWriteAckJSON(buf, t)
	case []ScopeCapacityOffender:
		return appendOffendersJSON(buf, t)
	default:
		b, _ := json.Marshal(v)
		return append(buf, b...)
	}
}

// appendWriteAckJSON matches writeAck's struct-tag-derived JSON
// shape: Scope/ID/Seq are dropped when zero-valued, Ts is always
// emitted. Mirrors what json.Marshal(writeAck{...}) produces.
func appendWriteAckJSON(buf []byte, a writeAck) []byte {
	buf = append(buf, '{')
	first := true
	if a.Scope != "" {
		buf = append(buf, `"scope":`...)
		buf = appendJSONString(buf, a.Scope)
		first = false
	}
	if a.ID != "" {
		if !first {
			buf = append(buf, ',')
		}
		buf = append(buf, `"id":`...)
		buf = appendJSONString(buf, a.ID)
		first = false
	}
	if a.Seq != 0 {
		if !first {
			buf = append(buf, ',')
		}
		buf = append(buf, `"seq":`...)
		buf = strconv.AppendUint(buf, a.Seq, 10)
		first = false
	}
	if !first {
		buf = append(buf, ',')
	}
	buf = append(buf, `"ts":`...)
	buf = strconv.AppendInt(buf, a.Ts, 10)
	return append(buf, '}')
}

// appendOffendersJSON serialises the scope-capacity 507 body. Each
// offender has all three fields (no omitempty), matching what
// json.Marshal would produce.
func appendOffendersJSON(buf []byte, offenders []ScopeCapacityOffender) []byte {
	buf = append(buf, '[')
	for i, o := range offenders {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = append(buf, `{"scope":`...)
		buf = appendJSONString(buf, o.Scope)
		buf = append(buf, `,"count":`...)
		buf = strconv.AppendInt(buf, int64(o.Count), 10)
		buf = append(buf, `,"cap":`...)
		buf = strconv.AppendInt(buf, int64(o.Cap), 10)
		buf = append(buf, '}')
	}
	return append(buf, ']')
}

func (api *API) handleHead(w http.ResponseWriter, r *http.Request) {
	started := time.Now()

	if r.Method != http.MethodGet {
		methodNotAllowed(w, started, http.MethodGet)
		return
	}

	q, err := parseScopeLimit(r, "/head")
	if err != nil {
		badRequest(w, started, err.Error())
		return
	}
	query := r.URL.Query()
	// /head reads forward by cursor only. Positional 'offset' addressing
	// lives on /tail exclusively because seq-based forward reads are stable
	// under /delete_up_to while position-based forward reads are not.
	if query.Has("offset") {
		badRequest(w, started, "the 'offset' parameter is not supported on /head; use 'after_seq' instead, or call /tail for position-based paging")
		return
	}

	// after_seq is optional: omitting it (or passing 0) returns the oldest
	// items from the scope, which covers the "give me the start of this
	// scope" case without requiring the client to know any seq values.
	var afterSeq uint64
	if raw := query.Get("after_seq"); raw != "" {
		afterSeq, err = strconv.ParseUint(raw, 10, 64)
		if err != nil {
			badRequest(w, started, "the 'after_seq' parameter must be a valid unsigned integer")
			return
		}
	}

	items, truncated, found := api.store.head(q.Scope, afterSeq, q.Limit)
	if !found {
		api.writeItemsMiss(w, started, nil)
		return
	}
	api.writeItemsHit(w, started, items, truncated, nil)
}

func (api *API) handleTail(w http.ResponseWriter, r *http.Request) {
	started := time.Now()

	if r.Method != http.MethodGet {
		methodNotAllowed(w, started, http.MethodGet)
		return
	}

	q, err := parseScopeLimit(r, "/tail")
	if err != nil {
		badRequest(w, started, err.Error())
		return
	}
	offset, err := normalizeOffset(r.URL.Query().Get("offset"))
	if err != nil {
		badRequest(w, started, err.Error())
		return
	}

	// /tail's wire shape carries `offset` between `count` and `truncated`
	// — the helpers slot `extra` at exactly that position.
	offsetField := orderedFields{kv{"offset", offset}}

	items, truncated, found := api.store.tail(q.Scope, q.Limit, offset)
	if !found {
		api.writeItemsMiss(w, started, offsetField)
		return
	}
	api.writeItemsHit(w, started, items, truncated, offsetField)
}

func (api *API) handleGet(w http.ResponseWriter, r *http.Request) {
	started := time.Now()

	if r.Method != http.MethodGet {
		methodNotAllowed(w, started, http.MethodGet)
		return
	}

	target, err := parseLookupTarget(r, "/get")
	if err != nil {
		badRequest(w, started, err.Error())
		return
	}

	var (
		item  Item
		found bool
	)
	if target.ByID {
		item, found = api.store.get(target.Scope, target.ID, 0)
	} else {
		item, found = api.store.get(target.Scope, "", target.Seq)
	}

	writeGetResponse(w, started, item, found)
}

// writeGetResponse assembles the /get JSON envelope manually and
// streams it in three writes (prefix, raw payload bytes, suffix).
// This bypasses the json.Marshal-then-splice path used by
// writeJSONWithMeta — the previous path copied the entire payload
// once during marshal and once again during the approx_response_mb
// splice. For 10 KiB+ payloads those two copies dominate /get's cost
// and account for most of the gap with /render. Eliminating both
// brings /get's throughput within ~10-20 % of /render even at large
// payload sizes.
//
// Wire-format compatibility:
//   - The envelope shape is identical to the previous orderedFields
//     emission: ok, hit, count, item, duration_us, approx_response_mb.
//   - For items in the reserved _events scope the payload-bearing
//     field is named "event" (matching Item.MarshalJSON's special
//     case); otherwise it is "payload".
//   - Field-omission rules mirror Item's struct tags: scope, id,
//     and seq are dropped when zero-valued; ts and the payload-
//     bearing field are always present on a hit.
//
// approx_response_mb is computed from a single estimated total
// rather than the convergence loop in marshalWithApproxSize. The
// value differs from the convergence-loop output by at most a few
// bytes — well under the 4-decimal MiB precision (~104 bytes per
// 0.0001) — and the user has signed off on that tolerance for the
// throughput win.
func writeGetResponse(w http.ResponseWriter, started time.Time, item Item, hit bool) {
	prefix := make([]byte, 0, 128)
	if !hit {
		prefix = append(prefix, `{"ok":true,"hit":false,"count":0,"item":null`...)
	} else {
		payloadKey := "payload"
		if item.Scope == EventsScopeName {
			payloadKey = "event"
		}

		prefix = append(prefix, `{"ok":true,"hit":true,"count":1,"item":{`...)
		first := true
		if item.Scope != "" {
			prefix = append(prefix, `"scope":`...)
			prefix = appendJSONString(prefix, item.Scope)
			first = false
		}
		if item.ID != "" {
			if !first {
				prefix = append(prefix, ',')
			}
			prefix = append(prefix, `"id":`...)
			prefix = appendJSONString(prefix, item.ID)
			first = false
		}
		if item.Seq != 0 {
			if !first {
				prefix = append(prefix, ',')
			}
			prefix = append(prefix, `"seq":`...)
			prefix = strconv.AppendUint(prefix, item.Seq, 10)
			first = false
		}
		if !first {
			prefix = append(prefix, ',')
		}
		prefix = append(prefix, `"ts":`...)
		prefix = strconv.AppendInt(prefix, item.Ts, 10)
		prefix = append(prefix, ',', '"')
		prefix = append(prefix, payloadKey...)
		prefix = append(prefix, '"', ':')
	}

	suffix := make([]byte, 0, 96)
	if hit {
		suffix = append(suffix, '}') // close item
	}
	suffix = append(suffix, `,"duration_us":`...)
	suffix = strconv.AppendInt(suffix, time.Since(started).Microseconds(), 10)

	// Single-pass approx_response_mb estimate. Tracks the actual
	// body size to within the width of the MB value itself (~8
	// bytes), which rounds away inside the 4-decimal precision.
	// strconv.AppendFloat with prec=4 matches MB.MarshalJSON's
	// fmt.Sprintf("%.4f", v) output exactly while skipping the
	// json.Marshal wrap and one allocation.
	estTotal := len(prefix) + len(item.Payload) + len(suffix) + 30
	mbVal := float64(estTotal) / 1048576.0
	suffix = append(suffix, `,"approx_response_mb":`...)
	suffix = strconv.AppendFloat(suffix, mbVal, 'f', 4, 64)
	suffix = append(suffix, '}', '\n')

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(prefix)
	if hit {
		// validatePayload rejects empty/null at write time so a stored
		// Item should never reach this branch with len(Payload) == 0.
		// Defensive: emit literal "null" instead of writing zero bytes
		// (which would produce malformed JSON: "...,"payload":}..."),
		// matching json.Marshal(RawMessage(nil))'s behaviour. ~1 ns
		// branch on the hot path.
		if len(item.Payload) == 0 {
			_, _ = w.Write([]byte("null"))
		} else {
			_, _ = w.Write(item.Payload)
		}
	}
	_, _ = w.Write(suffix)
}

// appendJSONString appends s to dst as a JSON-encoded string
// ("..."). Fast path for the common case where s contains no
// JSON-special bytes — single linear scan plus an inline copy
// with quote-wrap, no allocation. Slow path falls through to
// encoding/json so escape semantics (HTML-safe < style for
// <, >, &; control-char escaping; UTF-8 invalid-byte handling)
// stay byte-for-byte identical to the previous json.Marshal call.
//
// Specials list mirrors json.Encoder's default-on HTML-escape
// table; the validator already rejects control chars in scope/id
// so they only show up in pathological items injected via
// white-box tests, but the slow-path fallback is correct for
// them too.
func appendJSONString(dst []byte, s string) []byte {
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

// handleRender serves a single item as raw payload bytes with no JSON
// envelope. The use case is serving cached HTML/XML/JSON/text fragments
// directly from the cache (typically fronted by Caddy, nginx, or apache).
//
// Design rules — deliberately minimal:
//   - Hit and miss paths are envelope-free: 200 carries raw payload bytes,
//     404 carries an empty body. Both use Content-Type application/octet-stream
//     — a neutral default the fronting proxy is expected to override via its
//     own route config (e.g. `header Content-Type text/html`). The cache does
//     NOT sniff content or guess the real MIME type.
//   - Validation errors (missing scope, malformed seq, etc.) still use the
//     standard JSON error envelope. Those are developer-facing, not content-facing.
//   - If the stored payload is a JSON string (first non-whitespace byte is `"`),
//     one layer of JSON string-encoding is peeled so `"<html>..."` is served
//     as `<html>...` on the wire. All other JSON values (object, array, number,
//     bool) are written raw; the consumer is expected to parse them as JSON.
func (api *API) handleRender(w http.ResponseWriter, r *http.Request) {
	started := time.Now()

	if r.Method != http.MethodGet {
		methodNotAllowed(w, started, http.MethodGet)
		return
	}

	target, err := parseLookupTarget(r, "/render")
	if err != nil {
		badRequest(w, started, err.Error())
		return
	}

	writeMiss := func() {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusNotFound)
	}

	var (
		body  []byte
		found bool
	)
	if target.ByID {
		body, found = api.store.render(target.Scope, target.ID, 0)
	} else {
		body, found = api.store.render(target.Scope, "", target.Seq)
	}
	if !found {
		writeMiss()
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
