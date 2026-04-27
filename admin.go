package scopecache

import (
	"fmt"
	"net/http"
	"time"
)

// buildAdminCallSpecs returns the closed whitelist of paths /admin
// dispatches to, paired with their fixed HTTP method and raw handler.
// Wider than /multi_call's whitelist: includes operator-only operations
// (/warm, /rebuild, /wipe, /stats, /delete_scope_candidates,
// /delete_scope) that public callers do not see at all.
//
// Excluded: /help (text/plain, capability-independent), /render (raw
// bytes don't fit a JSON results array — would silently corrupt the
// envelope; operators reach /render directly on the public mux),
// /multi_call, /guarded, /admin (self-reference loops). See
// guardedflow.md §K.
func (api *API) buildAdminCallSpecs() map[string]subCallSpec {
	return map[string]subCallSpec{
		"/append":                  {http.MethodPost, api.handleAppend},
		"/get":                     {http.MethodGet, api.handleGet},
		"/head":                    {http.MethodGet, api.handleHead},
		"/tail":                    {http.MethodGet, api.handleTail},
		"/ts_range":                {http.MethodGet, api.handleTsRange},
		"/update":                  {http.MethodPost, api.handleUpdate},
		"/upsert":                  {http.MethodPost, api.handleUpsert},
		"/counter_add":             {http.MethodPost, api.handleCounterAdd},
		"/delete":                  {http.MethodPost, api.handleDelete},
		"/delete_up_to":            {http.MethodPost, api.handleDeleteUpTo},
		"/delete_scope":            {http.MethodPost, api.handleDeleteScope},
		"/delete_guarded":          {http.MethodPost, api.handleDeleteGuarded},
		"/warm":                    {http.MethodPost, api.handleWarm},
		"/rebuild":                 {http.MethodPost, api.handleRebuild},
		"/wipe":                    {http.MethodPost, api.handleWipe},
		"/stats":                   {http.MethodGet, api.handleStats},
		"/delete_scope_candidates": {http.MethodGet, api.handleDeleteScopeCandidates},
	}
}

// handleAdmin is the operator-elevated multi-call dispatcher. Same
// shape as /multi_call (calls-array body, results-array response) but:
//   - Wider whitelist (17 paths, includes operator-only ops).
//   - No body-level auth — gated by socket access + Caddyfile (see I).
//   - Reaches reserved scopes (`_*`) directly; no rewrite, no strip.
//   - The only path that creates scopes in the operator's normal flow
//     (e.g. provisioning `_guarded:<HMAC>:*` at token issuance).
//
// See guardedflow.md §K.
func (api *API) handleAdmin(w http.ResponseWriter, r *http.Request) {
	started := time.Now()

	if r.Method != http.MethodPost {
		methodNotAllowed(w, started)
		return
	}

	// Reuse /multi_call's body shape and limits — admin batches share the
	// dispatcher and therefore the same per-roundtrip work budget. The
	// per-request body cap is wider because /admin must accept a full
	// /rebuild body (see H in guardedflow.md).
	var req multiCallRequest
	bodyCap := bulkRequestBytesFor(api.store.maxStoreBytes)
	if err := decodeBody(w, r, bodyCap, &req); err != nil {
		badRequest(w, started, err.Error())
		return
	}
	if req.Calls == nil {
		badRequest(w, started, "the 'calls' field is required")
		return
	}
	calls := *req.Calls
	if len(calls) > api.store.maxMultiCallCount {
		badRequest(w, started, fmt.Sprintf("the 'calls' array has %d entries; the maximum is %d", len(calls), api.store.maxMultiCallCount))
		return
	}

	// Pre-flight response cap (see preflightResponseCap doc).
	if preflightResponseCap(w, started, len(calls), api.store.maxResponseBytes) {
		return
	}

	// Pre-validate the whitelist. Same stance as /multi_call: a bad
	// path in any slot rejects the whole batch up-front.
	for i, call := range calls {
		if _, ok := api.adminCallSpecs[call.Path]; !ok {
			badRequest(w, started, fmt.Sprintf("path '%s' (calls[%d]) is not allowed in /admin", call.Path, i))
			return
		}
	}

	// Pre-build subURLs and bodies before any side effect can land.
	// See prepareSubCalls in multi_call.go for the rationale.
	prepared, err := prepareSubCalls(calls, api.adminCallSpecs)
	if err != nil {
		badRequest(w, started, err.Error())
		return
	}

	// AdminContext flips the inner handler's rejectReservedScope check
	// off, letting operator sub-calls reach `_tokens`, `_guarded:*`,
	// `_counters_*` etc. directly. /admin is the ONLY way reserved
	// scopes are written to in normal operation.
	results, _ := api.dispatchPreparedCalls(prepared, batchDispatchOptions{AdminContext: true})

	writeJSONWithMeta(w, http.StatusOK, orderedFields{
		{"ok", true},
		{"count", len(results)},
		{"results", results},
	}, started)
}
