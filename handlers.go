// handlers.go is the shared HTTP-layer infrastructure for *API:
//
//   - error-class mapping (writeStoreCapacityError + the 4xx/5xx
//     responder family: badRequest, conflict, scopeFull, storeFull,
//     methodNotAllowed)
//   - body decoding (decodeBody)
//   - response shaping (orderedFields type + writeJSONWithDuration,
//     which builds the envelope manually via appendKVValue from
//     handlers_read.go)
//   - common request parsers (parseLookupTarget, parseScopeLimit)
//   - mux registration (RegisterRoutes)
//
// Per-endpoint families live in handlers_*.go siblings:
//
//   handlers_write.go    — /append, /upsert, /update, /counter_add
//   handlers_read.go     — /head, /tail, /get, /render
//   handlers_delete.go   — /delete, /delete_up_to, /delete_scope, /wipe
//   handlers_bulk.go     — /warm, /rebuild
//   handlers_observe.go  — /stats, /help

package scopecache

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"
)

// writeStoreCapacityError dispatches the three capacity-class
// errors the store returns on write paths:
//
//   - *ScopeFullError    — single-item over per-scope cap
//   - *ScopeCapacityError — bulk equivalent (carries offender list)
//   - *StoreFullError    — over the store-wide byte cap
//
// Returns true when matched + response written; caller should
// `return` immediately. Returns false on no match; caller falls
// back to handler-specific error handling.
//
// scopeForSFE is used only on the *ScopeFullError path (one-element
// offenders list). Pass "" for callers that cannot produce one.
func writeStoreCapacityError(w http.ResponseWriter, started time.Time, err error, scopeForSFE string) bool {
	var sfe *ScopeFullError
	if errors.As(err, &sfe) {
		scopeFull(w, started, []ScopeCapacityOffender{
			{Scope: scopeForSFE, Count: sfe.Count, Cap: sfe.Cap},
		})
		return true
	}
	var sce *ScopeCapacityError
	if errors.As(err, &sce) {
		scopeFull(w, started, sce.Offenders)
		return true
	}
	var stfe *StoreFullError
	if errors.As(err, &stfe) {
		storeFull(w, started, stfe)
		return true
	}
	return false
}

// writeMutationError dispatches the error-mapping pattern shared by
// /append, /upsert, /update, /warm, and /rebuild:
//
//   - ErrInvalidInput  → 400 with the wrapped message
//   - capacity classes → 507 via writeStoreCapacityError
//   - anything else    → 409 (orphan/race shape: *ScopeDetachedError,
//     etc.)
//
// scopeForSFE plumbs into writeStoreCapacityError's offender list —
// pass "" when the caller cannot produce *ScopeFullError.
//
// Caller invariant: err is non-nil. The helper writes exactly one
// response; caller must `return` immediately afterward.
func writeMutationError(w http.ResponseWriter, started time.Time, err error, scopeForSFE string) {
	if errors.Is(err, ErrInvalidInput) {
		badRequest(w, started, err.Error())
		return
	}
	if writeStoreCapacityError(w, started, err, scopeForSFE) {
		return
	}
	conflict(w, started, err.Error())
}

// decodeBody caps the request body at max bytes and decodes JSON into out.
// The MaxBytesReader guard runs at read time, so it protects against clients
// that omit Content-Length or stream chunked bodies just as much as sized ones.
// An exceeded-cap error is distinguished from a plain JSON syntax error so
// callers can return a meaningful message. A second Decode is used to reject
// trailing content (a second object or garbage after the first value), which
// json.Decoder would otherwise silently ignore.
func decodeBody(w http.ResponseWriter, r *http.Request, max int64, out interface{}) error {
	r.Body = http.MaxBytesReader(w, r.Body, max)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(out); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return errors.New("the request body exceeds the maximum allowed size of " +
				strconv.FormatInt(mbe.Limit, 10) + " bytes")
		}
		return errors.New("the request body must contain valid JSON")
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("the request body must contain exactly one JSON value")
	}
	return nil
}

// orderedFields is a JSON object whose keys are emitted in insertion order.
// encoding/json sorts map keys alphabetically, which scatters ok, errors,
// counts, and payloads through the output in whichever order the alphabet
// dictates. orderedFields lets every response put ok first, config/caps
// before aggregates, heavy or variable-size fields last, and duration_us
// at the very end — a shape a human eye (and a log scanner) can read at
// a glance.
type orderedFields []kv

type kv struct {
	K string
	V interface{}
}

func writeJSONWithDuration(w http.ResponseWriter, code int, payload orderedFields, started time.Time) {
	// Manual envelope build — replaces the json.Marshal + reflect path.
	// Each kv goes through appendKVValue (handlers_read.go), which has
	// fast paths for every value type the write-endpoint envelopes
	// carry (bool/int/int64/uint64/string/MB/writeAck/offenders) and
	// falls through to json.Marshal for unknown types so the wire
	// format stays byte-for-byte identical to the previous behaviour.
	//
	// duration_us is appended last and inline rather than via the
	// orderedFields-append-then-marshal pattern so the int conversion
	// doesn't need to flow through interface{} boxing.
	body := make([]byte, 0, 192)
	body = append(body, '{')
	for i, kv := range payload {
		if i > 0 {
			body = append(body, ',')
		}
		body = append(body, '"')
		body = append(body, kv.K...)
		body = append(body, '"', ':')
		body = appendKVValue(body, kv.V)
	}
	if len(payload) > 0 {
		body = append(body, ',')
	}
	body = append(body, `"duration_us":`...)
	body = strconv.AppendInt(body, time.Since(started).Microseconds(), 10)
	body = append(body, '}', '\n')

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_, _ = w.Write(body)
}

func badRequest(w http.ResponseWriter, started time.Time, message string) {
	writeJSONWithDuration(w, http.StatusBadRequest, orderedFields{
		{"ok", false},
		{"error", message},
	}, started)
}

func conflict(w http.ResponseWriter, started time.Time, message string) {
	writeJSONWithDuration(w, http.StatusConflict, orderedFields{
		{"ok", false},
		{"error", message},
	}, started)
}

// scopeFull responds with 507 Insufficient Storage and the full offender list.
// Used when an /append, /warm, or /rebuild would push one or more scopes past
// the per-scope capacity. The client is expected to drain (e.g. /delete_up_to
// or /delete_scope) or chunk the batch and retry.
func scopeFull(w http.ResponseWriter, started time.Time, offenders []ScopeCapacityOffender) {
	msg := "scope is at capacity"
	if len(offenders) > 1 {
		msg = "multiple scopes are at capacity"
	}
	writeJSONWithDuration(w, http.StatusInsufficientStorage, orderedFields{
		{"ok", false},
		{"error", msg},
		{"scopes", offenders},
	}, started)
}

// storeFull responds with 507 when the aggregate byte cap would be exceeded.
// The body carries the store-level totals (all in MiB, matching /stats) so a
// client can judge how much headroom remains and whether draining one scope
// will fix the next retry.
func storeFull(w http.ResponseWriter, started time.Time, e *StoreFullError) {
	writeJSONWithDuration(w, http.StatusInsufficientStorage, orderedFields{
		{"ok", false},
		{"error", "store is at byte capacity"},
		{"approx_store_mb", MB(e.StoreBytes)},
		{"added_mb", MB(e.AddedBytes)},
		{"max_store_mb", MB(e.Cap)},
	}, started)
}

// responseTooLarge writes the 507 envelope used by the cap-protected
// read endpoints (/head, /tail, /scopelist) when the marshalled
// body would exceed the per-response cap. Body shape mirrors the
// other 507 helpers (storeFull, scopeFull): {ok, error,
// approx_response_mb, max_response_mb, duration_us}.
//
// Side effects already applied by the handler are not rolled back.
// This matches every other 507 in the cache: 2xx is not durability,
// and 507 does not roll back. In practice the cap-protected
// endpoints are read-only, so there is nothing to roll back.
func responseTooLarge(w http.ResponseWriter, started time.Time, written, cap int64) {
	writeJSONWithDuration(w, http.StatusInsufficientStorage, orderedFields{
		{"ok", false},
		{"error", "the response would exceed the maximum allowed size"},
		{"approx_response_mb", MB(written)},
		{"max_response_mb", MB(cap)},
	}, started)
}

// methodNotAllowed responds 405 with an Allow header naming the
// method this endpoint accepts, per RFC 7231 §7.4.1 ("an origin
// server MUST generate an Allow header field in a 405 response").
// `allowed` is typically a single method (POST or GET); pass a
// comma-separated list if a future endpoint accepts multiple.
func methodNotAllowed(w http.ResponseWriter, started time.Time, allowed string) {
	w.Header().Set("Allow", allowed)
	writeJSONWithDuration(w, http.StatusMethodNotAllowed, orderedFields{
		{"ok", false},
		{"error", "the HTTP method is not allowed for this endpoint"},
	}, started)
}

// lookupTarget is the parsed form of /get's and /render's URL query:
// a scope plus exactly one of id or seq. Built by parseLookupTarget.
type lookupTarget struct {
	Scope string
	ByID  bool
	ID    string
	Seq   uint64
}

// parseLookupTarget pulls scope + exactly one of id/seq from the query
// string and validates each. Scope errors are labelled with the endpoint;
// the id/seq shape errors are endpoint-agnostic since the rule is the same
// on every single-item read.
func parseLookupTarget(r *http.Request, endpoint string) (lookupTarget, error) {
	query := r.URL.Query()
	scope := query.Get("scope")
	id := query.Get("id")
	seqStr := query.Get("seq")

	if err := validateScope(scope, endpoint); err != nil {
		return lookupTarget{}, err
	}

	hasID := id != ""
	hasSeq := seqStr != ""
	if hasID == hasSeq {
		return lookupTarget{}, errors.New("exactly one of 'id' or 'seq' must be provided")
	}

	if hasID {
		if err := validateID(id); err != nil {
			return lookupTarget{}, err
		}
		return lookupTarget{Scope: scope, ByID: true, ID: id}, nil
	}

	seq, err := strconv.ParseUint(seqStr, 10, 64)
	if err != nil {
		return lookupTarget{}, errors.New("the 'seq' parameter must be a valid unsigned integer")
	}
	// Cache-assigned seqs start at 1; seq=0 cannot match anything and
	// would silently miss. Reject it loudly so a caller passing 0
	// learns the value was malformed instead of getting hit:false.
	// /update and /delete enforce the same rule via validateIDOrSeq.
	if seq == 0 {
		return lookupTarget{}, errors.New("the 'seq' parameter must be a positive integer")
	}
	return lookupTarget{Scope: scope, Seq: seq}, nil
}

// scopeLimit is the parsed form of the scope+limit query pair used by every
// multi-item read (/head, /tail). Endpoint-specific params
// (offset, after_seq) are parsed by the handler itself — this
// helper deliberately stops at the common pair.
type scopeLimit struct {
	Scope string
	Limit int
}

// parseScopeLimit validates scope and normalizes limit in fixed
// order (scope first, then limit) to keep error ordering stable
// across handlers.
func parseScopeLimit(r *http.Request, endpoint string) (scopeLimit, error) {
	query := r.URL.Query()
	scope := query.Get("scope")
	if err := validateScope(scope, endpoint); err != nil {
		return scopeLimit{}, err
	}
	limit, err := normalizeLimit(query.Get("limit"))
	if err != nil {
		return scopeLimit{}, err
	}
	return scopeLimit{Scope: scope, Limit: limit}, nil
}

func (api *API) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/append", api.handleAppend)
	mux.HandleFunc("/update", api.handleUpdate)
	mux.HandleFunc("/upsert", api.handleUpsert)
	mux.HandleFunc("/counter_add", api.handleCounterAdd)
	mux.HandleFunc("/delete", api.handleDelete)
	mux.HandleFunc("/delete_up_to", api.handleDeleteUpTo)

	// /head and /tail enforce the per-response cap inside their
	// shared writer (writeJSONWithMetaCap, called from
	// writeItemsHit) — no outer middleware needed.
	mux.HandleFunc("/head", api.handleHead)
	mux.HandleFunc("/tail", api.handleTail)
	mux.HandleFunc("/get", api.handleGet)
	mux.HandleFunc("/render", api.handleRender)

	mux.HandleFunc("/wipe", api.handleWipe)
	mux.HandleFunc("/warm", api.handleWarm)
	mux.HandleFunc("/rebuild", api.handleRebuild)
	mux.HandleFunc("/delete_scope", api.handleDeleteScope)

	mux.HandleFunc("/stats", api.handleStats)
	mux.HandleFunc("/scopelist", api.handleScopeList)
	mux.HandleFunc("/help", api.handleHelp)
}
