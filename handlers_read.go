// Read handlers on the public mux:
//
//   - /head      — window after a seq cursor, oldest-first
//   - /tail      — window at the newest end, oldest-first within
//   - /get       — single-item lookup by id or seq, JSON envelope
//   - /render    — single-item raw payload, no JSON envelope
//
// /head and /tail share writeHeadResponse / writeTailResponse, which
// both route to writeItemsResponse so the wire shape (`ok`, `hit`,
// `count`, [`offset` for /tail], `truncated`, `items`,
// `approx_response_mb`) stays uniform across the list-returning read
// family. Pre-flight cap enforcement is done in the head/tail
// wrappers; the read-heat stamp (only on non-empty results) lives
// one layer down in Store.head / Store.tail.

package scopecache

import (
	"encoding/json"
	"net/http"
	"strconv"
)

// writeHeadResponse / writeTailResponse are the typed-struct entry
// points for the list-returning read endpoints. They call the shared
// writeItemsResponse builder with the right offset slot wired up
// (/tail carries it, /head does not). Pre-flight cap check applies
// only on the hit path where items are present.
func (api *API) writeHeadResponse(w http.ResponseWriter, resp HeadResponse) {
	if len(resp.Items) > 0 {
		if estimated := estimateMultiItemResponseBytes(resp.Items); estimated > api.maxResponseBytes {
			responseTooLarge(w, estimated, api.maxResponseBytes)
			return
		}
	}
	api.writeItemsResponse(w, resp.Items, resp.Truncated, nil)
}

func (api *API) writeTailResponse(w http.ResponseWriter, resp TailResponse) {
	if len(resp.Items) > 0 {
		if estimated := estimateMultiItemResponseBytes(resp.Items); estimated > api.maxResponseBytes {
			responseTooLarge(w, estimated, api.maxResponseBytes)
			return
		}
	}
	api.writeItemsResponse(w, resp.Items, resp.Truncated, &resp.Offset)
}

// writeItemsResponse builds the /head / /tail envelope manually in a
// single buffer and writes it once. The earlier json.Marshal+splice
// path copied the entire items array twice — once during marshal,
// once when splicing in the size-suffix — which dominated /tail's
// per-call cost at large payloads. The manual single-buffer path
// halves that allocation volume.
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
	items []Item,
	truncated bool,
	offset *int,
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

	// /tail carries offset between count and truncated; /head does not
	// (offset == nil).
	if offset != nil {
		buf = append(buf, `,"offset":`...)
		buf = strconv.AppendInt(buf, int64(*offset), 10)
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

	// Suffix: approx_response_mb (single-pass estimate). Computed
	// from the current buf length + the suffix bytes we are about
	// to append; tracks the actual marshalled size within MB-precision
	// rounding.
	estTotal := len(buf) + 30
	mbVal := float64(estTotal) / 1048576.0
	buf = append(buf, `,"approx_response_mb":`...)
	buf = strconv.AppendFloat(buf, mbVal, 'f', 4, 64)
	buf = append(buf, '}', '\n')

	// Authoritative cap on the actual marshalled body.
	if int64(len(buf)) > api.maxResponseBytes {
		responseTooLarge(w, int64(len(buf)), api.maxResponseBytes)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf)
}

// appendItemJSON appends item to buf as JSON, mirroring
// Item.MarshalJSON byte-for-byte: every item carries the full
// scope/id/seq/ts + payload-bearing key set. An item without a
// client-supplied id renders as `"id":null` rather than dropping
// the key — uniform-shape rule lets clients read item.id directly
// without a presence check. Items in the reserved _events scope
// rename "payload" to "event". Empty Payload (only reachable via
// white-box mutation; validatePayload blocks it on the write path)
// emits literal "null" instead of malformed `,"payload":}`.
func appendItemJSON(buf []byte, item Item) []byte {
	payloadKey := "payload"
	if item.Scope == EventsScopeName {
		payloadKey = "event"
	}

	buf = append(buf, `{"scope":`...)
	buf = appendJSONString(buf, item.Scope)
	buf = append(buf, `,"id":`...)
	if item.ID == "" {
		buf = append(buf, `null`...)
	} else {
		buf = appendJSONString(buf, item.ID)
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

func (api *API) handleHead(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}

	q, err := parseScopeLimit(r, "/head")
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	query := r.URL.Query()
	// /head reads forward by cursor only. Positional 'offset' addressing
	// lives on /tail exclusively because seq-based forward reads are stable
	// under /delete_up_to while position-based forward reads are not.
	if query.Has("offset") {
		badRequest(w, "the 'offset' parameter is not supported on /head; use 'after_seq' instead, or call /tail for position-based paging")
		return
	}

	// after_seq is optional: omitting it (or passing 0) returns the oldest
	// items from the scope, which covers the "give me the start of this
	// scope" case without requiring the client to know any seq values.
	var afterSeq uint64
	if raw := query.Get("after_seq"); raw != "" {
		afterSeq, err = strconv.ParseUint(raw, 10, 64)
		if err != nil {
			badRequest(w, "the 'after_seq' parameter must be a valid unsigned integer")
			return
		}
	}

	items, truncated, found := api.store.head(q.Scope, afterSeq, q.Limit)
	if !found {
		api.writeHeadResponse(w, HeadResponse{OK: true, Hit: false, Count: 0, Truncated: false, Items: nil})
		return
	}
	api.writeHeadResponse(w, HeadResponse{
		OK:        true,
		Hit:       len(items) > 0,
		Count:     len(items),
		Truncated: truncated,
		Items:     items,
	})
}

func (api *API) handleTail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}

	q, err := parseScopeLimit(r, "/tail")
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	offset, err := normalizeOffset(r.URL.Query().Get("offset"))
	if err != nil {
		badRequest(w, err.Error())
		return
	}

	items, truncated, found := api.store.tail(q.Scope, q.Limit, offset)
	if !found {
		api.writeTailResponse(w, TailResponse{OK: true, Hit: false, Count: 0, Offset: offset, Truncated: false, Items: nil})
		return
	}
	api.writeTailResponse(w, TailResponse{
		OK:        true,
		Hit:       len(items) > 0,
		Count:     len(items),
		Offset:    offset,
		Truncated: truncated,
		Items:     items,
	})
}

func (api *API) handleGet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}

	target, err := parseLookupTarget(r, "/get")
	if err != nil {
		badRequest(w, err.Error())
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

	if !found {
		writeGetResponse(w, GetResponse{OK: true, Hit: false, Count: 0, Item: nil})
		return
	}
	writeGetResponse(w, GetResponse{OK: true, Hit: true, Count: 1, Item: &item})
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
func writeGetResponse(w http.ResponseWriter, resp GetResponse) {
	// payload is hoisted to function scope so the post-prefix size
	// estimate and the final write-payload-bytes step can both see it.
	// Nil on miss; len(nil) == 0 keeps the estTotal math correct.
	var payload []byte

	prefix := make([]byte, 0, 128)
	if !resp.Hit {
		prefix = append(prefix, `{"ok":true,"hit":false,"count":0,"item":null`...)
	} else {
		item := *resp.Item
		payload = item.Payload
		payloadKey := "payload"
		if item.Scope == EventsScopeName {
			payloadKey = "event"
		}

		prefix = append(prefix, `{"ok":true,"hit":true,"count":1,"item":{"scope":`...)
		prefix = appendJSONString(prefix, item.Scope)
		prefix = append(prefix, `,"id":`...)
		if item.ID == "" {
			prefix = append(prefix, `null`...)
		} else {
			prefix = appendJSONString(prefix, item.ID)
		}
		prefix = append(prefix, `,"seq":`...)
		prefix = strconv.AppendUint(prefix, item.Seq, 10)
		prefix = append(prefix, `,"ts":`...)
		prefix = strconv.AppendInt(prefix, item.Ts, 10)
		prefix = append(prefix, ',', '"')
		prefix = append(prefix, payloadKey...)
		prefix = append(prefix, '"', ':')
	}

	suffix := make([]byte, 0, 96)
	if resp.Hit {
		suffix = append(suffix, '}') // close item
	}

	// Single-pass approx_response_mb estimate. Tracks the actual
	// body size to within the width of the MB value itself (~8
	// bytes), which rounds away inside the 4-decimal precision.
	estTotal := len(prefix) + len(payload) + len(suffix) + 30
	mbVal := float64(estTotal) / 1048576.0
	suffix = append(suffix, `,"approx_response_mb":`...)
	suffix = strconv.AppendFloat(suffix, mbVal, 'f', 4, 64)
	suffix = append(suffix, '}', '\n')

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(prefix)
	if resp.Hit {
		// validatePayload rejects empty/null at write time so a stored
		// Item should never reach this branch with len(Payload) == 0.
		// Defensive: emit literal "null" instead of writing zero bytes
		// (which would produce malformed JSON: "...,"payload":}...").
		if len(payload) == 0 {
			_, _ = w.Write([]byte("null"))
		} else {
			_, _ = w.Write(payload)
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
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}

	target, err := parseLookupTarget(r, "/render")
	if err != nil {
		badRequest(w, err.Error())
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
