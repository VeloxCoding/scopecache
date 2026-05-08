// Bulk write handlers on the public mux:
//
//   - /warm     — replace the scopes in the request, leave others alone
//   - /rebuild  — atomically replace the entire store
//
// Both decode an itemsRequest and route through store.replaceScopes /
// store.rebuildAll. Per-item shape validation lives in those store
// methods, so handlers decode, delegate, and map errors.
//
// /rebuild explicitly refuses an empty items array — the intent is
// ambiguous enough to reject (clear-everything vs missing-payload).
// Clients that genuinely want to clear the cache use /wipe.

package scopecache

import (
	"net/http"
	"time"
)

func (api *API) handleWarm(w http.ResponseWriter, r *http.Request) {
	started := time.Now()

	if r.Method != http.MethodPost {
		methodNotAllowed(w, started)
		return
	}

	var req itemsRequest
	if err := decodeBody(w, r, api.maxBulkBytes, &req); err != nil {
		badRequest(w, started, err.Error())
		return
	}

	grouped := groupItemsByScope(req.Items)
	replacedScopes, err := api.store.replaceScopes(grouped)
	if err != nil {
		// /warm produces only *ScopeCapacityError or *StoreFullError;
		// no *ScopeFullError, so scopeForSFE is unused.
		writeMutationError(w, started, err, "")
		return
	}

	writeJSONWithDuration(w, http.StatusOK, orderedFields{
		{"ok", true},
		{"count", len(req.Items)},
		{"replaced_scopes", replacedScopes},
	}, started)
}

func (api *API) handleRebuild(w http.ResponseWriter, r *http.Request) {
	started := time.Now()

	if r.Method != http.MethodPost {
		methodNotAllowed(w, started)
		return
	}

	var req itemsRequest
	if err := decodeBody(w, r, api.maxBulkBytes, &req); err != nil {
		badRequest(w, started, err.Error())
		return
	}

	// An empty items[] would wipe the entire store. The intent is
	// ambiguous enough to reject — missing payload, wrong key, or
	// serialization glitch all surface the same shape on the wire as
	// a deliberate clear-everything call. /wipe is the explicit route
	// for the latter. This guard is HTTP policy ("explicit-non-
	// empty-required"), not a per-item shape check; Go-API callers
	// of Gateway.Rebuild who want a wipe-shaped rebuild can pass an
	// empty map intentionally.
	if len(req.Items) == 0 {
		badRequest(w, started, "the 'items' array must not be empty for the '/rebuild' endpoint")
		return
	}

	grouped := groupItemsByScope(req.Items)
	rebuiltScopes, rebuiltItems, err := api.store.rebuildAll(grouped)
	if err != nil {
		// /rebuild produces only *ScopeCapacityError or *StoreFullError;
		// no *ScopeFullError, so scopeForSFE is unused.
		writeMutationError(w, started, err, "")
		return
	}

	writeJSONWithDuration(w, http.StatusOK, orderedFields{
		{"ok", true},
		{"count", len(req.Items)},
		{"rebuilt_scopes", rebuiltScopes},
		{"rebuilt_items", rebuiltItems},
	}, started)
}
