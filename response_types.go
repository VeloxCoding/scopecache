// response_types.go — typed envelope structs for every endpoint's
// success response, plus the four error-response shapes.
//
// Single source of truth for the HTTP wire format. Each handler builds
// its response struct, passes it to writeResponse, and the dispatcher
// picks the right serialisation:
//
//   - Read-side endpoints with cap-protected response bodies
//     (/get, /tail, /head, /scopelist) route to their existing manual
//     byte-builders; the builders now consume the struct directly.
//   - Everything else goes through json.Marshal(resp) + write.
//
// Field declaration order matches the historical inline-orderedFields
// emission order on each endpoint, so json.Marshal produces byte-
// identical output to the pre-refactor writeJSONWithDuration path —
// the wire format is unchanged.
//
// approx_response_mb is intentionally NOT a struct field. It depends
// on the final serialised body length, which is only known inside
// the fast-path byte-builder. The builder appends it after
// duration_us in the same place the historical code did. Read-side
// structs therefore describe the "data" half of the envelope; the
// fast-path adds the "size" half.

package scopecache

// --- Success: writes -----------------------------------------------

// AppendResponse is the body of a successful POST /append.
type AppendResponse struct {
	OK         bool     `json:"ok"`
	Item       writeAck `json:"item"`
	DurationUs int64    `json:"duration_us"`
}

// UpsertResponse is the body of a successful POST /upsert. `created`
// is true on first-write of the (scope,id) and false on payload
// replace.
type UpsertResponse struct {
	OK         bool     `json:"ok"`
	Created    bool     `json:"created"`
	Item       writeAck `json:"item"`
	DurationUs int64    `json:"duration_us"`
}

// UpdateResponse is the body of a successful POST /update. `hit` =
// `updated_count > 0`; both fields are kept for backward-compat with
// clients that branch on one or the other.
type UpdateResponse struct {
	OK           bool  `json:"ok"`
	Hit          bool  `json:"hit"`
	UpdatedCount int   `json:"updated_count"`
	DurationUs   int64 `json:"duration_us"`
}

// CounterAddResponse is the body of a successful POST /counter_add.
// `created` distinguishes first-touch (counter spawned) from in-place
// increment.
type CounterAddResponse struct {
	OK         bool  `json:"ok"`
	Created    bool  `json:"created"`
	Value      int64 `json:"value"`
	DurationUs int64 `json:"duration_us"`
}

// --- Success: deletes -----------------------------------------------

// DeleteResponse is the body of a successful POST /delete and
// /delete_up_to — both share the same wire shape.
type DeleteResponse struct {
	OK           bool  `json:"ok"`
	Hit          bool  `json:"hit"`
	DeletedCount int   `json:"deleted_count"`
	DurationUs   int64 `json:"duration_us"`
}

// DeleteScopeResponse is the body of a successful POST /delete_scope.
//
// NB: `hit` AND `deleted_scope` carry the SAME bool value (the
// "did-the-scope-exist" flag from store.deleteScope). Historical
// shape — two keys for the same data — preserved for Phase A
// byte-identical behaviour. Phase B is allowed to drop one of them.
type DeleteScopeResponse struct {
	OK           bool  `json:"ok"`
	Hit          bool  `json:"hit"`
	DeletedScope bool  `json:"deleted_scope"`
	DeletedItems int   `json:"deleted_items"`
	DurationUs   int64 `json:"duration_us"`
}

// WipeResponse is the body of a successful POST /wipe.
type WipeResponse struct {
	OK            bool  `json:"ok"`
	DeletedScopes int   `json:"deleted_scopes"`
	DeletedItems  int   `json:"deleted_items"`
	FreedMB       MB    `json:"freed_mb"`
	DurationUs    int64 `json:"duration_us"`
}

// --- Success: bulk --------------------------------------------------

// WarmResponse is the body of a successful POST /warm. `count` is the
// input item count (clients echo it back as a sanity check);
// `replaced_scopes` is the number of distinct scopes that were
// rewritten.
type WarmResponse struct {
	OK             bool  `json:"ok"`
	Count          int   `json:"count"`
	ReplacedScopes int   `json:"replaced_scopes"`
	DurationUs     int64 `json:"duration_us"`
}

// RebuildResponse is the body of a successful POST /rebuild.
type RebuildResponse struct {
	OK            bool  `json:"ok"`
	Count         int   `json:"count"`
	RebuiltScopes int   `json:"rebuilt_scopes"`
	RebuiltItems  int   `json:"rebuilt_items"`
	DurationUs    int64 `json:"duration_us"`
}

// --- Success: observability -----------------------------------------

// StatsResponse is the body of a successful GET /stats. Fields after
// `ok` are flattened straight from storeStats — same names as the
// orderedFields path used pre-refactor.
type StatsResponse struct {
	OK               bool                 `json:"ok"`
	ScopeCount       int                  `json:"scope_count"`
	TotalItems       int                  `json:"total_items"`
	ApproxStoreMB    MB                   `json:"approx_store_mb"`
	LastWriteTS      int64                `json:"last_write_ts"`
	EventsDropsTotal int64                `json:"events_drops_total"`
	ReservedScopes   []reservedScopeEntry `json:"reserved_scopes"`
	DurationUs       int64                `json:"duration_us"`
}

// --- Success: cap-protected reads ----------------------------------
//
// Fast-path read responses (Get/Tail/Head/ScopeList) describe ONLY
// the "data" half of the wire envelope. The "timing" half —
// duration_us and approx_response_mb — is emitted by the dedicated
// write*Response builder using a separate `started` parameter and a
// post-build buffer-size estimate. These structs therefore do NOT
// json.Marshal to the full wire output; their consumer is always the
// matching builder, never the generic writeJSONResponse path.

// GetResponse is the data half of a successful GET /get response.
// The wire-final envelope adds duration_us + approx_response_mb at
// emit time inside writeGetResponse.
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
// simple "ok:false, error, duration_us" shape. The variants that need
// extra fields (ScopeCapacityErrorResponse, StoreCapacityErrorResponse,
// ResponseTooLargeErrorResponse) are separate types so json.Marshal
// emits the right fields in the right order.
type ErrorResponse struct {
	OK         bool   `json:"ok"` // always false
	Error      string `json:"error"`
	DurationUs int64  `json:"duration_us"`
}

// ScopeCapacityErrorResponse is the 507 body emitted by scopeFull —
// one or more scopes are at their per-scope item cap.
type ScopeCapacityErrorResponse struct {
	OK         bool                    `json:"ok"` // always false
	Error      string                  `json:"error"`
	Scopes     []ScopeCapacityOffender `json:"scopes"`
	DurationUs int64                   `json:"duration_us"`
}

// StoreCapacityErrorResponse is the 507 body emitted by storeFull —
// the store-level aggregate byte cap would be exceeded.
type StoreCapacityErrorResponse struct {
	OK            bool   `json:"ok"` // always false
	Error         string `json:"error"`
	ApproxStoreMB MB     `json:"approx_store_mb"`
	AddedMB       MB     `json:"added_mb"`
	MaxStoreMB    MB     `json:"max_store_mb"`
	DurationUs    int64  `json:"duration_us"`
}

// ResponseTooLargeErrorResponse is the 507 body emitted by the
// cap-protected read endpoints (/head, /tail, /scopelist, /get) when
// the marshalled body would exceed the per-response cap.
type ResponseTooLargeErrorResponse struct {
	OK               bool   `json:"ok"` // always false
	Error            string `json:"error"`
	ApproxResponseMB MB     `json:"approx_response_mb"`
	MaxResponseMB    MB     `json:"max_response_mb"`
	DurationUs       int64  `json:"duration_us"`
}
