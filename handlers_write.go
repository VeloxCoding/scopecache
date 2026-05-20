// Single-item write handlers on the public mux:
//
//   - /append   — insert; rejects on dup id, capacity, or byte cap
//   - /upsert   — insert-or-replace by id; replace-whole-item semantics
//   - /update   — modify payload at an existing id or seq
//
// All three decode an Item body, run shape validation, route through
// the matching store method (appendOne / upsertOne / updateOne), and
// map errors uniformly via writeMutationError (handlers.go):
// ErrInvalidInput → 400, capacity → 507, else 409.

package scopecache

import "net/http"

// writeAck is the response shape /append and /upsert nest under
// "item". Mirrors Item's JSON layout for scope/id/seq/ts but
// deliberately excludes Payload — the client supplied it on the way
// in, and echoing it would double the wire cost on a 1 MiB write.
// ID is rendered via writeAckIDJSON so seq-only writes emit
// `"id":null` rather than dropping the key — matches the uniform
// item-shape rule applied across the read endpoints.
type writeAck struct {
	Scope string  `json:"scope"`
	ID    *string `json:"id"`
	Seq   uint64  `json:"seq"`
	Ts    int64   `json:"ts"`
}

// newWriteAck builds a writeAck from an Item, mapping an empty ID
// to a nil *string so json.Marshal emits `"id":null` rather than
// `"id":""`.
func newWriteAck(item Item) writeAck {
	var idPtr *string
	if item.ID != "" {
		id := item.ID
		idPtr = &id
	}
	return writeAck{Scope: item.Scope, ID: idPtr, Seq: item.Seq, Ts: item.Ts}
}

func (api *API) handleAppend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}

	var item Item
	if err := decodeBody(w, r, api.maxSingleBytes, &item); err != nil {
		badRequest(w, err.Error())
		return
	}

	origScope := item.Scope
	item, err := api.store.appendOne(item)
	if err != nil {
		writeMutationError(w, err, origScope)
		return
	}

	writeJSONResponse(w, http.StatusOK, AppendResponse{
		OK:      true,
		Created: true,
		Item:    newWriteAck(item),
	})
}

// handleUpsert creates a new item or replaces an existing one by scope + id.
// Unlike /append (which rejects duplicate ids) or /update (which soft-misses
// on absent items), /upsert always writes — making it the idempotent, retry-
// safe write path. Seq is preserved on replace and freshly assigned on create.
func (api *API) handleUpsert(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}

	var item Item
	if err := decodeBody(w, r, api.maxSingleBytes, &item); err != nil {
		badRequest(w, err.Error())
		return
	}

	origScope := item.Scope
	result, created, err := api.store.upsertOne(item)
	if err != nil {
		writeMutationError(w, err, origScope)
		return
	}

	// Same item-with-no-payload shape as /append; see comment there. Seq
	// is the pre-existing seq on a replace and the freshly-assigned seq
	// on a create.
	writeJSONResponse(w, http.StatusOK, UpsertResponse{
		OK:      true,
		Created: created,
		Item:    newWriteAck(result),
	})
}

func (api *API) handleUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}

	var item Item
	if err := decodeBody(w, r, api.maxSingleBytes, &item); err != nil {
		badRequest(w, err.Error())
		return
	}

	updated, err := api.store.updateOne(item)
	if err != nil {
		// /update only ever sees *StoreFullError on the cap path
		// (existing-item replace can grow byte size); scopeForSFE is
		// unused.
		writeMutationError(w, err, "")
		return
	}

	writeJSONResponse(w, http.StatusOK, UpdateResponse{
		OK:      true,
		Created: false,
		Count:   updated,
	})
}
