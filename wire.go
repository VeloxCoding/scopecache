// wire.go — public byte-builders that emit each endpoint's success
// envelope into a caller-supplied buffer. Byte-identical to the
// matching HTTP response body (including the trailing newline and
// approx_response_mb where applicable).
//
// The HTTP handlers keep their own private writers — those carry
// transport-specific optimisations (3-write splice for /get, sync.Pool
// for /head and /tail). The public byte-builders here exist for
// in-process callers (Caddy add-ons, the FrankenPHP extension, future
// SDK consumers) that need the same wire shape but write to a buffer
// instead of an http.ResponseWriter. The byte_identity_test.go suite
// asserts the two paths stay in sync.
//
// Naming convention: `Append*JSON(buf, ...) []byte` matches the
// stdlib `strconv.AppendInt` style — the caller owns the buffer, can
// pool it, can pre-grow it. None of these allocate; the slice grow on
// `append` is the caller's concern.

package scopecache

import "strconv"

// AppendItemJSON appends item to buf as JSON, byte-identical to
// Item.MarshalJSON. Items in the reserved _events scope rename
// "payload" to "event" and "uuid" to "event_uuid"; empty Payload
// renders as `null` (defensive — validatePayload rejects empty on
// write).
func AppendItemJSON(buf []byte, item Item) []byte {
	payloadKey := "payload"
	uuidKey := "uuid"
	if item.Scope == EventsScopeName {
		payloadKey = "event"
		uuidKey = "event_uuid"
	}

	buf = append(buf, `{"scope":`...)
	buf = AppendJSONString(buf, item.Scope)
	buf = append(buf, `,"id":`...)
	if item.ID == "" {
		buf = append(buf, `null`...)
	} else {
		buf = AppendJSONString(buf, item.ID)
	}
	buf = append(buf, `,"seq":`...)
	buf = strconv.AppendUint(buf, item.Seq, 10)
	buf = append(buf, `,"ts":`...)
	buf = strconv.AppendInt(buf, item.Ts, 10)
	buf = append(buf, ',', '"')
	buf = append(buf, uuidKey...)
	buf = append(buf, '"', ':')
	buf = AppendJSONString(buf, item.UUID)
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

// AppendJSONString appends s to dst as a JSON-encoded string. Fast
// path: linear scan, inline quote-wrap, no allocation. Slow path
// (any byte in 0x00..0x1f or " \ < > &) falls through to jsonMarshal
// so escape semantics stay byte-identical to encoding/json defaults.
func AppendJSONString(dst []byte, s string) []byte {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c == '"' || c == '\\' || c == '<' || c == '>' || c == '&' {
			b, _ := jsonMarshal(s)
			return append(dst, b...)
		}
	}
	dst = append(dst, '"')
	dst = append(dst, s...)
	return append(dst, '"')
}

// AppendGetResponseJSON appends a /get envelope to buf. item==nil is
// the miss shape (`hit:false, count:0, item:null`); non-nil emits the
// hit shape around *item. Includes approx_response_mb and trailing
// newline — byte-identical to writeGetResponse's HTTP output.
func AppendGetResponseJSON(buf []byte, item *Item) []byte {
	// Two-stage build: first the prefix+item+suffix-up-to-mb-field, then
	// the mb estimate from the partial length. Mirrors writeGetResponse
	// but uses a single buffer instead of the 3-write splice (the splice
	// only matters when the payload is large enough to dominate; for the
	// in-process caller, slice growth is cheap).
	startLen := len(buf)
	if item == nil {
		buf = append(buf, `{"ok":true,"hit":false,"count":0,"item":null`...)
	} else {
		buf = append(buf, `{"ok":true,"hit":true,"count":1,"item":`...)
		buf = AppendItemJSON(buf, *item)
	}

	estTotal := len(buf) - startLen + 30
	mbVal := float64(estTotal) / 1048576.0
	buf = append(buf, `,"approx_response_mb":`...)
	buf = strconv.AppendFloat(buf, mbVal, 'f', 4, 64)
	return append(buf, '}', '\n')
}

// AppendHeadResponseJSON appends a /head envelope. Same shape as
// AppendTailResponseJSON minus the offset field.
func AppendHeadResponseJSON(buf []byte, items []Item, truncated bool) []byte {
	return appendItemsEnvelopeJSON(buf, items, truncated, nil)
}

// AppendTailResponseJSON appends a /tail envelope. offset is the
// position cursor (0 for un-paged reads).
func AppendTailResponseJSON(buf []byte, items []Item, truncated bool, offset int) []byte {
	return appendItemsEnvelopeJSON(buf, items, truncated, &offset)
}

// appendItemsEnvelopeJSON is the shared builder for /head and /tail.
// offset==nil means /head shape (no offset field).
func appendItemsEnvelopeJSON(buf []byte, items []Item, truncated bool, offset *int) []byte {
	startLen := len(buf)

	buf = append(buf, `{"ok":true,"hit":`...)
	if len(items) > 0 {
		buf = append(buf, `true`...)
	} else {
		buf = append(buf, `false`...)
	}
	buf = append(buf, `,"count":`...)
	buf = strconv.AppendInt(buf, int64(len(items)), 10)

	if offset != nil {
		buf = append(buf, `,"offset":`...)
		buf = strconv.AppendInt(buf, int64(*offset), 10)
	}

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
		buf = AppendItemJSON(buf, items[i])
	}
	buf = append(buf, ']')

	estTotal := len(buf) - startLen + 30
	mbVal := float64(estTotal) / 1048576.0
	buf = append(buf, `,"approx_response_mb":`...)
	buf = strconv.AppendFloat(buf, mbVal, 'f', 4, 64)
	return append(buf, '}', '\n')
}

// AppendScopeListResponseJSON appends a /scopelist envelope. Each
// entry's ten scalar fields are emitted in the same order as
// ScopeListEntry's struct tags.
func AppendScopeListResponseJSON(buf []byte, entries []ScopeListEntry, truncated bool) []byte {
	startLen := len(buf)

	buf = append(buf, `{"ok":true,"hit":`...)
	if len(entries) > 0 {
		buf = append(buf, `true`...)
	} else {
		buf = append(buf, `false`...)
	}
	buf = append(buf, `,"count":`...)
	buf = strconv.AppendInt(buf, int64(len(entries)), 10)
	buf = append(buf, `,"truncated":`...)
	if truncated {
		buf = append(buf, `true`...)
	} else {
		buf = append(buf, `false`...)
	}
	buf = append(buf, `,"scopes":[`...)
	for i := range entries {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = appendScopeListEntryJSON(buf, entries[i])
	}
	buf = append(buf, ']')

	estTotal := len(buf) - startLen + 30
	mbVal := float64(estTotal) / 1048576.0
	buf = append(buf, `,"approx_response_mb":`...)
	buf = strconv.AppendFloat(buf, mbVal, 'f', 4, 64)
	return append(buf, '}', '\n')
}

// MarshalEnvelope is the public wrapper around scopecache's chosen
// JSON encoder. Use it from add-ons / extensions to marshal one of
// the *Response types from response_types.go into the wire format.
// Internally this routes through goccy/go-json so the bytes match
// what writeJSONResponse on the HTTP side would emit.
//
// Reads (Get/Head/Tail/ScopeList) have dedicated AppendNNResponseJSON
// builders that are faster and emit approx_response_mb; use those
// instead of MarshalEnvelope for those four response types.
func MarshalEnvelope(v any) ([]byte, error) {
	return jsonMarshal(v)
}

// --- WriteAck public alias ------------------------------------------

// WriteAck is the public alias for the per-item write-acknowledgement
// embedded in /append, /upsert response bodies (the "item" field).
type WriteAck = writeAck

// NewWriteAck builds a WriteAck from an Item, mapping an empty ID to
// `"id":null` (mirrors the private newWriteAck).
func NewWriteAck(item Item) WriteAck {
	return newWriteAck(item)
}
