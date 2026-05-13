// response_types.go — typed envelope structs for every endpoint's
// success response, plus the three error-response shapes.
//
// Single source of truth for the HTTP wire format. Each handler builds
// its response struct and passes it to writeJSONResponse, which calls
// json.Marshal. The struct's declared field order is preserved by
// encoding/json, so wire output matches the response shapes documented
// in docs/scopecache-core-rfc.md §6.
//
// Cap-protected reads (/get, /tail, /head, /scopelist) keep their
// dedicated byte-builders for payload-copy avoidance; those builders
// consume the typed struct directly and emit approx_response_mb
// (computed from final buffer length, not stored in the struct).

package scopecache

// --- Success: writes -----------------------------------------------

// AppendResponse is the body of a successful POST /append. /append
// always creates a new item, so `created` is always true — emitted
// for envelope uniformity with /upsert and /counter_add.
type AppendResponse struct {
	OK      bool     `json:"ok"`
	Created bool     `json:"created"` // always true
	Item    writeAck `json:"item"`
}

// UpsertResponse is the body of a successful POST /upsert. `created`
// is true on first-write of the (scope,id) and false on payload
// replace.
type UpsertResponse struct {
	OK      bool     `json:"ok"`
	Created bool     `json:"created"`
	Item    writeAck `json:"item"`
}

// UpdateResponse is the body of a successful POST /update. /update
// only ever modifies an existing item, so `created` is always false
// — emitted for envelope uniformity. `count` is the number of items
// the update touched (0 on miss, 1 on hit).
type UpdateResponse struct {
	OK      bool `json:"ok"`
	Created bool `json:"created"` // always false
	Count   int  `json:"count"`
}

// CounterAddResponse is the body of a successful POST /counter_add.
// `created` distinguishes first-touch (counter spawned) from in-place
// increment.
type CounterAddResponse struct {
	OK      bool  `json:"ok"`
	Created bool  `json:"created"`
	Value   int64 `json:"value"`
}

// --- Success: deletes -----------------------------------------------

// DeleteResponse is the body of a successful POST /delete and
// /delete_up_to — both share the same wire shape.
type DeleteResponse struct {
	OK    bool `json:"ok"`
	Hit   bool `json:"hit"`
	Count int  `json:"count"`
}

// DeleteScopeResponse is the body of a successful POST /delete_scope.
//
// `hit` reflects "did the scope exist before this call" — distinct
// from /delete and /delete_up_to where `hit` means "anything was
// deleted". On /delete_scope an existing-but-already-empty scope
// still counts as a hit (the scope is gone after the call).
type DeleteScopeResponse struct {
	OK    bool `json:"ok"`
	Hit   bool `json:"hit"`
	Count int  `json:"count"` // items in the deleted scope
}

// WipeResponse is the body of a successful POST /wipe.
type WipeResponse struct {
	OK      bool `json:"ok"`
	Scopes  int  `json:"scopes"`
	Items   int  `json:"items"`
	FreedMB MB   `json:"freed_mb"`
}

// --- Success: bulk --------------------------------------------------

// WarmResponse is the body of a successful POST /warm. `scopes` is
// the number of distinct scopes that were rewritten.
type WarmResponse struct {
	OK     bool `json:"ok"`
	Scopes int  `json:"scopes"`
}

// RebuildResponse is the body of a successful POST /rebuild.
type RebuildResponse struct {
	OK     bool `json:"ok"`
	Scopes int  `json:"scopes"`
	Items  int  `json:"items"`
}

// --- Success: observability -----------------------------------------

// StatsResponse is the body of a successful GET /stats. Fields after
// `ok` are flattened straight from storeStats.
type StatsResponse struct {
	OK               bool                 `json:"ok"`
	Scopes           int                  `json:"scopes"`
	Items            int                  `json:"items"`
	ApproxStoreMB    MB                   `json:"approx_store_mb"`
	LastWriteTS      int64                `json:"last_write_ts"`
	EventsDropsTotal int64                `json:"events_drops_total"`
	ReservedScopes   []reservedScopeEntry `json:"reserved_scopes"`
}

// --- Success: cap-protected reads ----------------------------------
//
// Fast-path read responses (Get/Tail/Head/ScopeList) describe ONLY
// the "data" half of the wire envelope. The "size" half —
// approx_response_mb — is emitted by the dedicated write*Response
// builder using a post-build buffer-size estimate. These structs
// therefore do NOT json.Marshal to the full wire output; their
// consumer is always the matching builder, never the generic
// writeJSONResponse path.

// GetResponse is the data half of a successful GET /get response.
// The wire-final envelope adds approx_response_mb at emit time inside
// writeGetResponse.
type GetResponse struct {
	OK    bool  `json:"ok"`
	Hit   bool  `json:"hit"`
	Count int   `json:"count"`
	Item  *Item `json:"item"` // nil → `"item":null` (miss)
}

// TailResponse is the data half of a successful GET /tail response.
type TailResponse struct {
	OK        bool   `json:"ok"`
	Hit       bool   `json:"hit"`
	Count     int    `json:"count"`
	Offset    int    `json:"offset"`
	Truncated bool   `json:"truncated"`
	Items     []Item `json:"items"`
}

// HeadResponse is the data half of a successful GET /head response.
// Same shape as TailResponse minus the offset field.
type HeadResponse struct {
	OK        bool   `json:"ok"`
	Hit       bool   `json:"hit"`
	Count     int    `json:"count"`
	Truncated bool   `json:"truncated"`
	Items     []Item `json:"items"`
}

// ScopeListResponse is the data half of a successful GET /scopelist.
type ScopeListResponse struct {
	OK        bool             `json:"ok"`
	Hit       bool             `json:"hit"`
	Count     int              `json:"count"`
	Truncated bool             `json:"truncated"`
	Scopes    []scopeListEntry `json:"scopes"`
}

// --- Error responses ------------------------------------------------

// ErrorResponse is the body of 400 / 405 / 409 / 507 responses for the
// simple "ok:false, error" shape. The variants that need extra fields
// (ScopeCapacityErrorResponse, StoreCapacityErrorResponse,
// ResponseTooLargeErrorResponse) are separate types so json.Marshal
// emits the right fields in the right order.
type ErrorResponse struct {
	OK    bool   `json:"ok"` // always false
	Error string `json:"error"`
}

// ScopeCapacityErrorResponse is the 507 body emitted by scopeFull —
// one or more scopes are at their per-scope item cap.
type ScopeCapacityErrorResponse struct {
	OK     bool                    `json:"ok"` // always false
	Error  string                  `json:"error"`
	Scopes []ScopeCapacityOffender `json:"scopes"`
}

// StoreCapacityErrorResponse is the 507 body emitted by storeFull —
// the store-level aggregate byte cap would be exceeded.
type StoreCapacityErrorResponse struct {
	OK            bool   `json:"ok"` // always false
	Error         string `json:"error"`
	ApproxStoreMB MB     `json:"approx_store_mb"`
	AddedMB       MB     `json:"added_mb"`
	MaxStoreMB    MB     `json:"max_store_mb"`
}

// ResponseTooLargeErrorResponse is the 507 body emitted by the
// cap-protected read endpoints (/head, /tail, /scopelist, /get) when
// the marshalled body would exceed the per-response cap.
type ResponseTooLargeErrorResponse struct {
	OK               bool   `json:"ok"` // always false
	Error            string `json:"error"`
	ApproxResponseMB MB     `json:"approx_response_mb"`
	MaxResponseMB    MB     `json:"max_response_mb"`
}
