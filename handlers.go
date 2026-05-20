// handlers.go is the shared HTTP-layer infrastructure for *API:
//
//   - error-class mapping (writeStoreCapacityError + the 4xx/5xx
//     responder family: badRequest, conflict, scopeFull, storeFull,
//     methodNotAllowed)
//   - body decoding (decodeBody)
//   - response shaping (writeJSONResponse — json.Marshal of the
//     typed envelope structs in response_types.go; field declaration
//     order = wire emission order)
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
	"errors"
	"io"
	"net/http"
	"strconv"
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
func writeStoreCapacityError(w http.ResponseWriter, err error, scopeForSFE string) bool {
	var sfe *ScopeFullError
	if errors.As(err, &sfe) {
		scopeFull(w, []ScopeCapacityOffender{
			{Scope: scopeForSFE, Count: sfe.Count, Cap: sfe.Cap},
		})
		return true
	}
	var sce *ScopeCapacityError
	if errors.As(err, &sce) {
		scopeFull(w, sce.Offenders)
		return true
	}
	var stfe *StoreFullError
	if errors.As(err, &stfe) {
		storeFull(w, stfe)
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
func writeMutationError(w http.ResponseWriter, err error, scopeForSFE string) {
	if errors.Is(err, ErrInvalidInput) {
		badRequest(w, err.Error())
		return
	}
	if writeStoreCapacityError(w, err, scopeForSFE) {
		return
	}
	conflict(w, err.Error())
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
	dec := jsonNewDecoder(r.Body)
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

// writeJSONResponse is the canonical envelope-writer for every typed
// response struct in [response_types.go]. The struct's declared field
// order — preserved by encoding/json — produces wire output that
// matches the historical orderedFields path byte-for-byte. Inner
// types (Item, MB, writeAck, ScopeCapacityOffender) carry their own
// MarshalJSON or json tags so json.Marshal reproduces their shapes
// too.
//
// Per-endpoint cap-protected reads (/get, /tail, /head, /scopelist)
// do NOT go through here: they need to append approx_response_mb
// after the marshalled body and they enforce a per-response byte cap;
// both live in their own writeXxxResponse builders that consume the
// typed struct directly.
//
// On a marshal error (should be impossible for the typed structs we
// own, but harmless to defend against) we emit a minimal valid
// JSON 500 so the connection still returns parseable bytes.
func writeJSONResponse(w http.ResponseWriter, code int, resp any) {
	body, err := jsonMarshal(resp)
	if err != nil {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"ok":false,"error":"internal: response marshal failed"}` + "\n"))
		return
	}
	body = append(body, '\n')
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_, _ = w.Write(body)
}

func badRequest(w http.ResponseWriter, message string) {
	writeJSONResponse(w, http.StatusBadRequest, ErrorResponse{
		OK:    false,
		Error: message,
	})
}

func conflict(w http.ResponseWriter, message string) {
	writeJSONResponse(w, http.StatusConflict, ErrorResponse{
		OK:    false,
		Error: message,
	})
}

// scopeFull responds with 507 Insufficient Storage and the full offender list.
// Used when an /append, /warm, or /rebuild would push one or more scopes past
// the per-scope capacity. The client is expected to drain (e.g. /delete_up_to
// or /delete_scope) or chunk the batch and retry.
func scopeFull(w http.ResponseWriter, offenders []ScopeCapacityOffender) {
	msg := "scope is at capacity"
	if len(offenders) > 1 {
		msg = "multiple scopes are at capacity"
	}
	writeJSONResponse(w, http.StatusInsufficientStorage, ScopeCapacityErrorResponse{
		OK:     false,
		Error:  msg,
		Scopes: offenders,
	})
}

// storeFull responds with 507 when the aggregate byte cap would be exceeded.
// The body carries the store-level totals (all in MiB, matching /stats) so a
// client can judge how much headroom remains and whether draining one scope
// will fix the next retry.
func storeFull(w http.ResponseWriter, e *StoreFullError) {
	writeJSONResponse(w, http.StatusInsufficientStorage, StoreCapacityErrorResponse{
		OK:            false,
		Error:         "store is at byte capacity",
		ApproxStoreMB: MB(e.StoreBytes),
		AddedMB:       MB(e.AddedBytes),
		MaxStoreMB:    MB(e.Cap),
	})
}

// responseTooLarge writes the 507 envelope used by the cap-protected
// read endpoints (/head, /tail, /scopelist) when the marshalled
// body would exceed the per-response cap.
//
// Side effects already applied by the handler are not rolled back.
// This matches every other 507 in the cache: 2xx is not durability,
// and 507 does not roll back. In practice the cap-protected
// endpoints are read-only, so there is nothing to roll back.
func responseTooLarge(w http.ResponseWriter, written, cap int64) {
	writeJSONResponse(w, http.StatusInsufficientStorage, ResponseTooLargeErrorResponse{
		OK:               false,
		Error:            "the response would exceed the maximum allowed size",
		ApproxResponseMB: MB(written),
		MaxResponseMB:    MB(cap),
	})
}

// methodNotAllowed responds 405 with an Allow header naming the
// method this endpoint accepts, per RFC 7231 §7.4.1 ("an origin
// server MUST generate an Allow header field in a 405 response").
// `allowed` is typically a single method (POST or GET); pass a
// comma-separated list if a future endpoint accepts multiple.
func methodNotAllowed(w http.ResponseWriter, allowed string) {
	w.Header().Set("Allow", allowed)
	writeJSONResponse(w, http.StatusMethodNotAllowed, ErrorResponse{
		OK:    false,
		Error: "the HTTP method is not allowed for this endpoint",
	})
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
