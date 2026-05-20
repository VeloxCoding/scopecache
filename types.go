// types.go owns cross-cutting type definitions: Config + EventsMode +
// {Events,Inbox}Config (operator knobs every adapter fills), Item
// (the on-the-wire shape, with Ts contract, renderBytes optimisation,
// and counter side-channel), the error types handlers convert to
// 4xx/5xx responses, and the package-level capacity constants
// (ScopeMaxItems, MaxStoreMiB, MaxItemBytes, …).
//
// Cross-cutting invariants other files inherit from here:
//
//   - Bytes are the core arithmetic unit. Adapters convert their
//     MiB/KiB-facing operator config to bytes at the boundary; the
//     core never re-converts. The MB JSON helper renders bytes back
//     to MiB-with-4-decimals on observability surfaces.
//   - Reserved scopes (`_events`, `_inbox`) have their names defined
//     here; the rejection-and-recreation contract lives in store.go.
//   - approxItemSize is the single byte-cost function admission
//     control consults. Counters charge counterCellOverhead instead
//     of len(Payload) so increments stay lock-free; renderBytes is
//     counted so HTML-rendered stores report their real footprint.

package scopecache

import (
	"encoding/json"
	"fmt"
	"strconv"
	"sync/atomic"
	"time"
)

// Defaults applied by Config.WithDefaults; the runtime caps the
// adapter knobs feed in (env vars, Caddyfile keys) override these.
const (
	// DefaultLimit is the response size when a client omits ?limit.
	DefaultLimit = 1000
	// MaxLimit is the ceiling on ?limit; higher values are clamped.
	MaxLimit = 10000
	// ScopeMaxItems is the per-scope item-count default. Writes that
	// would exceed this are rejected with 507.
	ScopeMaxItems = 100000
	// MaxStoreMiB is the store-wide approxItemSize default in MiB.
	// Writes past this are rejected with 507.
	MaxStoreMiB = 100
	// MaxItemBytes is the per-item approxItemSize default in bytes
	// (overhead + scope + id + payload).
	MaxItemBytes = 1 << 20
	// MaxScopeBytes / MaxIDBytes are the validator's max byte length
	// for the scope and id fields.
	MaxScopeBytes = 256
	MaxIDBytes    = 256

	// InboxMaxItemBytes is the default per-item cap for the reserved
	// `_inbox` scope, in bytes. 64 KiB matches the "fan-in event drop"
	// shape `_inbox` is designed for — small structured records that
	// drainers process in bulk. Overridable via
	// SCOPECACHE_INBOX_MAX_ITEM_KB (integer KiB).
	InboxMaxItemBytes = 64 << 10

	// eventsItemEnvelopeOverhead is the slack added to the largest
	// upstream user-cap to derive the `_events` scope's per-item cap.
	// An event entry wraps the user payload in a JSON envelope (op,
	// scope, id?, seq, ts, plus framing); 1 KiB is generous over the
	// actual ~150 B of envelope so future field additions don't force
	// a knob bump.
	//
	// `_events`'s per-item cap is derived as
	// `max(MaxItemBytes, Inbox.MaxItemBytes) + eventsItemEnvelopeOverhead`
	// in newStore, not exposed as a knob: an event entry must always
	// be at least as wide as the largest user-write that could
	// produce it, otherwise an Inbox configured with
	// MaxItemBytes > MaxItemBytes would silently drop its events on
	// the auto-populate path. Operators tune the upstream caps;
	// `_events` follows.
	eventsItemEnvelopeOverhead = 1024

	// EventsScopeName is the reserved scope name for the auto-populated
	// write-event stream. Pre-created at newStore time and re-created
	// at /wipe / /rebuild time. Scope-level destructive ops
	// (/delete_scope, /warm-target, /rebuild-input) reject the name;
	// item-level ops (/append, /delete, /delete_up_to, /get, /head,
	// /tail, /render) pass through so the drainer pattern (subscribe
	// → tail → process → delete_up_to) keeps working.
	EventsScopeName = "_events"

	// InboxScopeName is the reserved scope name for application-side
	// fan-in ingestion. Same pre-creation + reservation contract as
	// EventsScopeName. Apps /append into _inbox; the cache itself
	// never auto-writes to it.
	InboxScopeName = "_inbox"

	// MissHeader is set to "true" on a response whose lookup found
	// no item: a /get, /render, /tail, /head, /update, or /delete
	// miss. It lifts the body's `hit:false` to the header layer so
	// proxies and middleware can branch without parsing the body.
	// Absent on a hit, on errors, and on bulk/observability endpoints.
	MissHeader = "Scopecache-Miss"

	// singleRequestBytesOverhead is the headroom added on top of the configured
	// per-item cap to produce the request body cap for single-item endpoints
	// (/append, /update, /upsert, /delete, /delete_scope, /delete_up_to,
	// /counter_add). Covers JSON framing — keys ("scope","id","payload"),
	// quotes, colons, braces — on top of the item payload. The scope and id
	// bytes themselves are already counted inside approxItemSize, so the
	// framing overhead is tiny and constant. 4 KiB leaves generous slack.
	singleRequestBytesOverhead = 4096

	// bulkRequestBytesOverhead is the headroom added on top of the configured
	// store cap to produce the per-request cap for /warm and /rebuild. See
	// bulkRequestBytesFor: a full cache must fit into a single bulk request,
	// plus JSON framing (keys, quotes, separators, wrapper object).
	bulkRequestBytesOverhead = 16 << 20 // 16 MiB

	// MaxCounterValue is the largest absolute value a /counter_add operation
	// may observe or produce. Matches the JavaScript safe-integer range
	// (2^53 - 1), so counter values marshalled into JSON round-trip through
	// every client without loss of precision. Applies to `by`, the existing
	// counter value, and the result of the addition.
	MaxCounterValue int64 = (1 << 53) - 1 // 9,007,199,254,740,991
)

// Config bundles the cache-internal capacity knobs every adapter
// fills before constructing a Gateway. WithDefaults replaces non-
// positive fields with the compile-time defaults; `Config{}` is "all
// defaults".
//
// `_events` is exempt from ScopeMaxItems (observability; byte-cap
// only) and derives its per-item cap from
// `max(MaxItemBytes, Inbox.MaxItemBytes) + eventsItemEnvelopeOverhead`
// — the max() is what keeps a large `_inbox` write from silently
// dropping its event when `Inbox.MaxItemBytes > MaxItemBytes`.
// `_inbox` itself uses the operator-tunable Inbox.{MaxItems,
// MaxItemBytes} independently of the user-scope caps.
type Config struct {
	ScopeMaxItems int
	MaxStoreBytes int64
	MaxItemBytes  int64

	Events EventsConfig
	Inbox  InboxConfig
}

// EventsMode controls auto-populate of the reserved `_events` scope
// on every successful mutation:
//
//   - EventsModeOff    (default) — no auto-populate; zero write-path
//     overhead. Opt in when a drainer is ready.
//   - EventsModeNotify           — metadata only (op, scope, id?, seq,
//     ts). Smallest entries; sufficient for drainers that re-fetch
//     from cache state on wake-up.
//   - EventsModeFull             — metadata + action-payload.
//     Sufficient for drainers that replicate state without re-querying.
//
// "Action-payload" = the inputs the caller sent. /counter_add logs
// `by`, not the post-add value — the event stream stays replay-able
// and matches the WAL discipline downstream sinks expect.
type EventsMode int

const (
	EventsModeOff    EventsMode = iota // 0 — default; no auto-populate
	EventsModeNotify                   // 1 — events without payload
	EventsModeFull                     // 2 — events with payload
)

// String returns the canonical lowercase form (off | notify | full),
// matching the values SCOPECACHE_EVENTS_MODE / events_mode accept.
// Unknown values render as "unknown" so a forgotten new mode shows up
// in observability output rather than silently rendering as "off".
func (m EventsMode) String() string {
	switch m {
	case EventsModeOff:
		return "off"
	case EventsModeNotify:
		return "notify"
	case EventsModeFull:
		return "full"
	default:
		return "unknown"
	}
}

// ParseEventsMode parses the string form (off | notify | full) into
// the typed enum. Empty maps to EventsModeOff so adapter code passes
// through "unset" without special-casing.
func ParseEventsMode(s string) (EventsMode, error) {
	switch s {
	case "", "off":
		return EventsModeOff, nil
	case "notify":
		return EventsModeNotify, nil
	case "full":
		return EventsModeFull, nil
	default:
		return EventsModeOff, fmt.Errorf("invalid events_mode %q (expected: off | notify | full)", s)
	}
}

// EventsConfig holds the operator knob for the `_events` scope.
// Per-item cap and item-count cap are intentionally not configurable:
// the per-event cap stays coupled to MaxItemBytes (+ envelope slack)
// so large user-writes never 507 on the auto-populate path, and the
// item-count cap is exempt because `_events` is observability, not
// user state.
type EventsConfig struct {
	Mode EventsMode
}

// InboxConfig holds operator knobs for the reserved `_inbox` scope
// (app-populated fan-in). Both caps are independent of the user-
// scope globals: per-item is typically 64 KiB (small structured
// records) where user-scopes may be MiB; item-count stays tunable so
// drainer cadence sets the ceiling, not user-scope economics.
type InboxConfig struct {
	MaxItems     int
	MaxItemBytes int64
}

// WithDefaults returns a copy of c with non-positive numeric fields
// replaced by the compile-time defaults. Called by NewGateway so
// `Config{}` is a valid "all defaults" input.
//
// MaxStoreBytes is also clamped up to reservedScopesOverhead so the
// reserved scopes always fit at boot — only fires for absurdly
// small caps (under 2 KiB), realistic MB/GB caps are untouched.
//
// Events.Mode outside the recognised range (Off / Notify / Full)
// gets clamped to Off. The HTTP and env-var parsers reject unknown
// strings via ParseEventsMode, but a pure Go-API caller could pass
// an out-of-range int value directly. Defaulting silently to Off is
// the safe call: the alternative — letting an unrecognised value
// fall through to the "anything not Off is Full" branch in
// emitEvent — would silently emit events with payload, the most
// privacy-sensitive mode.
func (c Config) WithDefaults() Config {
	if c.ScopeMaxItems <= 0 {
		c.ScopeMaxItems = ScopeMaxItems
	}
	if c.MaxStoreBytes <= 0 {
		c.MaxStoreBytes = int64(MaxStoreMiB) << 20
	}
	if c.MaxStoreBytes < reservedScopesOverhead {
		c.MaxStoreBytes = reservedScopesOverhead
	}
	if c.MaxItemBytes <= 0 {
		c.MaxItemBytes = int64(MaxItemBytes)
	}
	if c.Inbox.MaxItems <= 0 {
		c.Inbox.MaxItems = c.ScopeMaxItems
	}
	if c.Inbox.MaxItemBytes <= 0 {
		c.Inbox.MaxItemBytes = int64(InboxMaxItemBytes)
	}
	switch c.Events.Mode {
	case EventsModeOff, EventsModeNotify, EventsModeFull:
		// recognised
	default:
		c.Events.Mode = EventsModeOff
	}
	return c
}

// MB is an int64 byte count that serializes to JSON as a number in MiB with
// 4 decimals (e.g. 79845 bytes → 0.0762). One display unit across every
// user-facing surface (/stats, 507 responses) keeps clients from juggling
// units. The underlying byte value is preserved for arithmetic.
type MB int64

// fmt.Sprintf goes through fmt's reflection / interface{}-boxing
// machinery — ~150 ns per call on a typical AMD core. strconv.AppendFloat
// is a direct float-formatter and shaves ~100 ns per MB-bearing
// response. The "%.4f" → ('f', 4) mapping is byte-for-byte identical
// for the non-negative finite values MB ever represents (bytes
// divided by 1048576; never NaN/Inf, never negative).
func (m MB) MarshalJSON() ([]byte, error) {
	return strconv.AppendFloat(make([]byte, 0, 24), float64(m)/1048576.0, 'f', 4, 64), nil
}

// Item is the on-the-wire shape of a cached entry. Payload is
// json.RawMessage: the cache stores it as opaque bytes (no parsing
// of arbitrary user content) but enforces three wire-level
// invariants on the raw bytes — must be syntactically valid JSON,
// must be valid UTF-8, and must not be empty or literal `null`.
// Documented inspection paths beyond those invariants:
// renderBytes precompute for JSON-string shortcuts in /render,
// and counter parsing in /counter_add.
//
// Ts is cache-owned (time.Now().UnixMicro()) and refreshed on every
// write that touches the item; clients must not supply ts (400 on
// any write endpoint). The field is observability only — not
// searchable, not indexed, not used for ordering. Microsecond (not
// millisecond) granularity keeps two writes inside the same ms
// distinguishable, which matters for ordered /inbox draining and
// counter "last activity" stamping under burst load.
type Item struct {
	Scope   string          `json:"scope,omitempty"`
	ID      string          `json:"id,omitempty"`
	Seq     uint64          `json:"seq,omitempty"`
	Ts      int64           `json:"ts"`
	Payload json.RawMessage `json:"payload"` // see MarshalJSON: serialised as `event` for _events items

	// renderBytes is the JSON-string-decoded form of Payload, set at
	// write-time for payloads whose first non-whitespace byte is `"`,
	// nil otherwise (object/array/number/bool/null pass through to
	// /render verbatim). Trades a one-shot Unmarshal + alloc at write
	// for a saved Unmarshal at every /render hit.
	//
	// Unexported so encoding/json never marshals it: in-process state,
	// not part of the wire format.
	renderBytes []byte

	// counter is non-nil iff this item was created or promoted by
	// /counter_add. cell.{value,ts} are the source of truth; Payload
	// and Ts on the surrounding Item are stale by construction and
	// re-rendered from the cell at read time (materialiseCounter in
	// buffer_read.go).
	//
	// Side-channel rather than in-place Payload mutation because the
	// fast path runs under b.mu.RLock — rewriting []byte under a read
	// lock would race with concurrent readers. atomic.Int64 fields on
	// the cell give us lock-free increment + CAS-max ts under RLock;
	// Payload bytes are only ever produced fresh on the read boundary.
	counter *counterCell
}

// MarshalJSON keeps the universal Item shape (scope/id/seq/ts +
// payload-bearing field) but renames the payload-bearing key to
// `event` when the item lives in the reserved `_events` scope. The
// stored bytes there are a writeEvent envelope (see events.go), not
// a user-supplied payload — calling that field `payload` would put
// the same word at two nesting levels with two different meanings
// (outer = envelope, inner = client-content). Renaming the outer
// key removes the ambiguity: `payload` always means "the bytes the
// client originally stored", at every level it appears.
//
// `id` is always emitted: items with no client-supplied id render
// as `"id":null` rather than dropping the key. Uniform-shape rule
// means every item on the wire has the same key set; clients can
// read `item.id` directly without a presence check.
//
// Implementation note: emitting via two struct literals (one per
// scope) keeps the generated JSON well-defined. encoding/json's
// reflection path produces consistent field-order matching the
// struct declaration, so /tail _events output remains stable across
// Go versions.
// Hand-rolled to bypass json.Marshal's reflection of an anonymous
// struct. The reflection path was the dominant cost in any json.Marshal
// call that included an Item — ~630 ns of CPU per item on top of the
// raw byte cost. The hot HTTP read endpoints (/get, /head, /tail,
// /scopelist) already side-step Item.MarshalJSON entirely via the
// appendItemJSON byte-builder; this rewrite extends the same fast
// path to every OTHER caller (tests, future code, any path that
// marshals an envelope containing Item) so the slow reflection path
// is never reachable.
//
// Wire shape is byte-for-byte identical to the previous anonymous-
// struct emission. The validation layer (validation.go) constrains
// scope/id charsets such that appendJSONString's fast path is
// expected to hit on every realistic call; the json.Marshal fallback
// inside appendJSONString preserves byte-exact escape semantics for
// any unexpected input.
func (i Item) MarshalJSON() ([]byte, error) {
	payloadKey := `"payload":`
	if i.Scope == EventsScopeName {
		payloadKey = `"event":`
	}
	buf := make([]byte, 0, 128)
	buf = append(buf, `{"scope":`...)
	buf = AppendJSONString(buf, i.Scope)
	buf = append(buf, `,"id":`...)
	if i.ID == "" {
		buf = append(buf, `null`...)
	} else {
		buf = AppendJSONString(buf, i.ID)
	}
	buf = append(buf, `,"seq":`...)
	buf = strconv.AppendUint(buf, i.Seq, 10)
	buf = append(buf, `,"ts":`...)
	buf = strconv.AppendInt(buf, i.Ts, 10)
	buf = append(buf, ',')
	buf = append(buf, payloadKey...)
	if len(i.Payload) == 0 {
		buf = append(buf, `null`...)
	} else {
		buf = append(buf, i.Payload...)
	}
	return append(buf, '}'), nil
}

// counterCell is the lock-free state for a counter item. value is
// the current integer, ts the microsecond timestamp of the most
// recent increment. Both atomic so /counter_add's fast path runs
// under b.mu.RLock without serialising on the scope's write lock.
//
// atomic.Int64 is non-copyable per `go vet`, so Item.counter is a
// pointer: map/slice copies of Item share the same cell. That's the
// point — an increment on any copy is observed by every reader.
type counterCell struct {
	value atomic.Int64
	ts    atomic.Int64
}

// counterCellOverhead is the byte cost approxItemSize charges for a
// counter item's payload-related state, in place of len(Payload) +
// len(renderBytes) for regular items. Two components:
//
//   - 24 bytes for the maximum decimal int64 representation
//     ("-9223372036854775808" is 20 chars, plus slack). The cell's
//     value is rendered fresh on every read so we charge the worst
//     case once at creation and never re-reserve on increment.
//   - 32 bytes for the *counterCell heap allocation itself
//     (two atomic.Int64 fields + struct overhead).
//
// Pre-reserving the worst case is what lets counter increments run
// lock-free: every increment that bumps the value's digit count would
// otherwise need to take Lock to reserve the byte delta. With this
// fixed overhead, increments never touch byte accounting.
const counterCellOverhead = 24 + 32

type deleteRequest struct {
	Scope string `json:"scope"`
	ID    string `json:"id,omitempty"`
	Seq   uint64 `json:"seq,omitempty"`
}

type deleteScopeRequest struct {
	Scope string `json:"scope"`
}

type deleteUpToRequest struct {
	Scope  string `json:"scope"`
	MaxSeq uint64 `json:"max_seq"`
}

// counterAddRequest is the body of /counter_add. By is a pointer so
// the handler can distinguish a missing field from an explicit zero;
// the latter is rejected with 400 because it has no observable
// effect.
type counterAddRequest struct {
	Scope string `json:"scope"`
	ID    string `json:"id"`
	By    *int64 `json:"by"`
}

type itemsRequest struct {
	Items []Item `json:"items"`
}

// ScopeCapacityOffender is one entry in a 507 response body: which scope
// overflowed, how many items the request/state held, and the active cap.
type ScopeCapacityOffender struct {
	Scope string `json:"scope"`
	Count int    `json:"count"`
	Cap   int    `json:"cap"`
}

// ScopeFullError is returned by scopeBuffer.appendItem when the buffer is at
// capacity. The handler converts it to a 507 response with the scope name.
type ScopeFullError struct {
	Count int
	Cap   int
}

func (e *ScopeFullError) Error() string {
	return "scope is at capacity"
}

// ScopeCapacityError is returned by Store.replaceScopes and Store.rebuildAll
// when one or more scopes in a batch would exceed the per-scope cap. The
// batch is rejected as a whole (no partial apply).
type ScopeCapacityError struct {
	Offenders []ScopeCapacityOffender
}

func (e *ScopeCapacityError) Error() string {
	if len(e.Offenders) == 1 {
		o := e.Offenders[0]
		return "scope '" + o.Scope + "' is at capacity"
	}
	return "multiple scopes are at capacity"
}

// StoreFullError is returned when a write would push the store's aggregate
// approxItemSize past the configured byte cap. AddedBytes is the net delta
// the rejected write attempted to commit; it may be larger than the free
// budget even when StoreBytes itself is under the cap (e.g. a /warm that
// replaces a small scope with a large one).
type StoreFullError struct {
	StoreBytes int64
	AddedBytes int64
	Cap        int64
}

func (e *StoreFullError) Error() string {
	return "store is at byte capacity"
}

// CounterPayloadError is returned by scopeBuffer.counterAdd when the existing
// item at scope+id cannot participate in a counter operation: its payload is
// not a JSON number, not an integer, or outside the allowed ±MaxCounterValue
// range. The handler converts it to 409 Conflict.
type CounterPayloadError struct {
	Reason string
}

func (e *CounterPayloadError) Error() string {
	return e.Reason
}

// CounterOverflowError is returned by scopeBuffer.counterAdd when the
// resulting value would exceed ±MaxCounterValue. The handler converts it to
// 400 Bad Request — the caller supplied a `by` that combined with the current
// value would push the counter outside the JS-safe integer range.
type CounterOverflowError struct {
	Current int64
	By      int64
}

func (e *CounterOverflowError) Error() string {
	return "the counter operation would exceed the allowed range of ±(2^53-1)"
}

// ScopeDetachedError is returned by a scope-buffer write method when the
// buffer has been unlinked from its Store (by /delete_scope, /wipe, or
// /rebuild) between the handler's getScope/getOrCreateScope call and the
// buffer-level mutation. The write would otherwise succeed into an orphan
// buffer that no subsequent reader can reach, so the caller is told the
// write did not take effect. The handler converts this to 409 Conflict.
type ScopeDetachedError struct{}

func (e *ScopeDetachedError) Error() string {
	return "the scope was deleted while the request was in flight; please retry"
}

func nowUnixMicro() int64 {
	return time.Now().UnixMicro()
}

func approxItemSize(item Item) int64 {
	var n int64
	n += 32
	n += int64(len(item.Scope))
	n += int64(len(item.ID))
	n += 8 // Seq
	n += 8 // Ts (always set, plain int64)
	if item.counter != nil {
		// Counter items charge a fixed overhead (cell heap + max int64
		// string) instead of len(Payload). The actual stored Payload
		// bytes on a counter item are stale by construction — readers
		// materialise from cell.value at the boundary, so we don't
		// account for them. See counterCellOverhead's comment for the
		// rationale of pre-reserving the worst case at creation.
		n += counterCellOverhead
		return n
	}
	n += int64(len(item.Payload))
	// renderBytes is heap-resident only for JSON-string payloads
	// (precomputed at write time so /render skips a per-hit
	// json.Unmarshal). Counted against the cap so approx_store_mb
	// reflects real memory; without this, a string-payload-heavy
	// store would under-report by the renderBytes total.
	n += int64(len(item.renderBytes))
	return n
}

// Multi-item read pre-flight constants. Used to short-circuit /head
// and /tail with a 507 BEFORE the marshaller allocates the full
// response body — writeJSONWithMetaCap is the post-flight authoritative
// cap, but it only fires after the body is in memory. A misbehaving
// client hitting /tail?limit=10000 against 1 MiB items would otherwise
// allocate the entire body before the cap rejects it.
//
// Both constants are STRICT lower bounds — overestimating per-item or
// envelope cost would reject legitimate calls. The post-flight cap
// remains authoritative; pre-flight is a memory optimisation, not a
// correctness gate.
const (
	// multiItemEnvelopeMinBytes is the minimum byte cost of the outer
	// JSON envelope ({ok, hit, count, truncated, items,
	// approx_response_mb}). 80 bytes is below the smallest possible
	// envelope encoding for either handler, even when count
	// is single-digit and the float fields are at minimum width.
	multiItemEnvelopeMinBytes = 80
	// multiItemPerItemMinBytes is the minimum on-wire cost of one
	// item's JSON skeleton (keys, quotes, separators, seq digits).
	// Even a stripped-down `{"scope":"","seq":1,"payload":null}` is
	// 35 bytes; 25 is below that floor and below every realistic
	// item produced by the cache.
	multiItemPerItemMinBytes = 25
)

// estimateMultiItemResponseBytes returns a strict lower bound on the
// JSON-marshalled response size for a /head or /tail payload with
// the given items. Used by the pre-flight check; see
// multiItemEnvelopeMinBytes for the rationale.
//
// Counts only bytes that must appear on the wire: the envelope
// minimum, a fixed per-item skeleton cost, and the scope/id/payload
// values written verbatim (JSON-string escaping only ADDS bytes
// during marshal, never removes — so raw len() is an underestimate
// of the encoded form, which is what we need).
func estimateMultiItemResponseBytes(items []Item) int64 {
	n := int64(multiItemEnvelopeMinBytes) + int64(len(items))*multiItemPerItemMinBytes
	for i := range items {
		n += int64(len(items[i].Scope))
		n += int64(len(items[i].ID))
		n += int64(len(items[i].Payload))
	}
	return n
}

// bulkRequestBytesFor returns the per-request body cap for /warm and
// /rebuild, derived from the configured store cap. A full store must
// always fit into a single bulk request; the extra 10% plus
// bulkRequestBytesOverhead covers JSON framing (keys, quotes, array
// separators, wrapper object) and provides a non-zero floor for very
// small store caps.
//
// Saturating add: caddymodule + standalone validate operator config
// against maxConfigMB before unit-shift, but a pure Go-API caller
// can hand NewGateway any int64 value for MaxStoreBytes including
// math.MaxInt64. Plain `+` would wrap to a negative request cap;
// addClampedInt64 saturates at math.MaxInt64 so MaxBytesReader
// keeps a sane upper bound.
func bulkRequestBytesFor(maxStoreBytes int64) int64 {
	return addClampedInt64(addClampedInt64(maxStoreBytes, maxStoreBytes/10), bulkRequestBytesOverhead)
}

// singleRequestBytesFor returns the per-request body cap for
// single-item endpoints, derived from the configured per-item cap.
// The item cap is a semantic limit on approxItemSize (enforced in
// the validator); this request cap is a DoS guardrail on the raw
// HTTP body (enforced by MaxBytesReader). The 4 KiB overhead covers
// JSON framing (keys, quotes, braces) on top of the item bytes —
// scope and id are already counted inside approxItemSize, so the
// framing is tiny and constant.
//
// Saturating add for the same reason as bulkRequestBytesFor: a pure
// Go-API caller can hand any int64 value for MaxItemBytes.
func singleRequestBytesFor(maxItemBytes int64) int64 {
	return addClampedInt64(maxItemBytes, singleRequestBytesOverhead)
}
