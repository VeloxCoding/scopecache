// Delete handlers on the public mux:
//
//   - /delete         — single-item delete by scope+id or scope+seq
//   - /delete_up_to   — bulk-delete every item with seq <= max_seq
//   - /delete_scope   — remove a whole scope
//   - /wipe           — reset the store; reserved scopes are recreated
//
// These endpoints have no built-in auth/admin tier; access control
// belongs at the adapter, proxy, socket layer, or in an access-policy
// addon. Store-side validation still protects reserved scopes where
// the operation would violate their contract (e.g. /delete_scope on
// `_events` or `_inbox` returns 400).

package scopecache

import (
	"errors"
	"net/http"
	"time"
)

func (api *API) handleDelete(w http.ResponseWriter, r *http.Request) {
	started := time.Now()

	if r.Method != http.MethodPost {
		methodNotAllowed(w, started, http.MethodPost)
		return
	}

	var req deleteRequest
	if err := decodeBody(w, r, api.maxSingleBytes, &req); err != nil {
		badRequest(w, started, err.Error())
		return
	}

	deleted, err := api.store.deleteOne(req.Scope, req.ID, req.Seq)
	if err != nil {
		if errors.Is(err, ErrInvalidInput) {
			badRequest(w, started, err.Error())
			return
		}
		// *ScopeDetachedError: the scope was wiped/deleted/rebuilt
		// between the lookup and the mutation. Surface as 409 — same
		// stance as /append, /upsert, /update, /counter_add. A retry
		// will see the new state (possibly miss, possibly a fresh
		// scope with no such id).
		conflict(w, started, err.Error())
		return
	}

	writeJSONResponse(w, http.StatusOK, DeleteResponse{
		OK:           true,
		Hit:          deleted > 0,
		DeletedCount: deleted,
		DurationUs:   time.Since(started).Microseconds(),
	})
}

func (api *API) handleDeleteUpTo(w http.ResponseWriter, r *http.Request) {
	started := time.Now()

	if r.Method != http.MethodPost {
		methodNotAllowed(w, started, http.MethodPost)
		return
	}

	var req deleteUpToRequest
	if err := decodeBody(w, r, api.maxSingleBytes, &req); err != nil {
		badRequest(w, started, err.Error())
		return
	}

	deleted, err := api.store.deleteUpTo(req.Scope, req.MaxSeq)
	if err != nil {
		if errors.Is(err, ErrInvalidInput) {
			badRequest(w, started, err.Error())
			return
		}
		// Same orphan-detect rationale as handleDelete above.
		conflict(w, started, err.Error())
		return
	}

	writeJSONResponse(w, http.StatusOK, DeleteResponse{
		OK:           true,
		Hit:          deleted > 0,
		DeletedCount: deleted,
		DurationUs:   time.Since(started).Microseconds(),
	})
}

func (api *API) handleDeleteScope(w http.ResponseWriter, r *http.Request) {
	started := time.Now()

	if r.Method != http.MethodPost {
		methodNotAllowed(w, started, http.MethodPost)
		return
	}

	var req deleteScopeRequest
	if err := decodeBody(w, r, api.maxSingleBytes, &req); err != nil {
		badRequest(w, started, err.Error())
		return
	}

	deletedItems, deleted, err := api.store.deleteScope(req.Scope)
	if err != nil {
		if errors.Is(err, ErrInvalidInput) {
			badRequest(w, started, err.Error())
			return
		}
		conflict(w, started, err.Error())
		return
	}

	writeJSONResponse(w, http.StatusOK, DeleteScopeResponse{
		OK:           true,
		Hit:          deleted,
		DeletedScope: deleted,
		DeletedItems: deletedItems,
		DurationUs:   time.Since(started).Microseconds(),
	})
}

// handleWipe clears the entire store: every scope, every item, every byte
// reservation. It is the store-wide complement of /delete_scope — destructive
// in one call, with no request body. The response carries the counts and
// freed bytes so the client can verify what was released.
//
// This is *not* an eviction policy: the cache never wipes on its own.
// /wipe exists because a client-side "for each scope: /delete_scope" is
// N calls and not atomic, whereas a server-side wipe is one lock and one
// map replacement.
func (api *API) handleWipe(w http.ResponseWriter, r *http.Request) {
	started := time.Now()

	if r.Method != http.MethodPost {
		methodNotAllowed(w, started, http.MethodPost)
		return
	}

	// /wipe takes no request body. We still cap what Go's auto-drain might
	// read so a misbehaving client cannot pin server memory by pushing a
	// large body to a body-less endpoint.
	r.Body = http.MaxBytesReader(w, r.Body, 1024)

	deletedScopes, deletedItems, freedBytes := api.store.wipe()

	writeJSONResponse(w, http.StatusOK, WipeResponse{
		OK:            true,
		DeletedScopes: deletedScopes,
		DeletedItems:  deletedItems,
		FreedMB:       MB(freedBytes),
		DurationUs:    time.Since(started).Microseconds(),
	})
}
