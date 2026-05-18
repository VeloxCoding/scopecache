// Observability + meta handlers:
//
//   - /stats     — store-wide aggregate snapshot
//   - /scopelist — per-scope detail with prefix filter and cursor pagination
//   - /help      — text/plain pointer to the canonical RFC
//
// /help is the only handler in the file that returns text/plain rather
// than JSON — it is documentation the cache hands out about itself, not
// observability data.
//
// /stats is aggregate-only for user-managed scopes — at 100k+ scopes
// the per-scope enumeration dominated /stats latency and the response
// routinely blew past practical client/proxy limits. Per-scope detail
// for user scopes lives at /scopelist, which pages it via a stable
// alphabetical cursor.
//
// reserved_scopes is the small, fixed exception: `_events` and
// `_inbox` are cache infrastructure (drainer-backlog and fan-in queue
// depth) that operators monitor independently of user scopes. The set
// is bounded by the reserved-scope list (currently 2 entries), so
// /stats stays O(1) regardless of total scope count.

package scopecache

import (
	"net/http"
	"strconv"
)

func (api *API) handleStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}

	st := api.store.stats()

	// /stats is a pure state endpoint: aggregate scope/item counts,
	// current byte usage, a freshness tick, and the per-scope state
	// of the cache's reserved infrastructure scopes. Static config
	// (DefaultLimit, MaxLimit, per-scope/per-item/store caps) lives
	// on /help, not here — those values do not change between calls
	// and are not useful on every poll. last_write_ts lets a polling
	// client decide "anything changed since I last looked?" with a
	// single integer comparison instead of refetching state.
	writeJSONResponse(w, http.StatusOK, StatsResponse{
		OK:               true,
		Scopes:           st.Scopes,
		Items:            st.Items,
		ApproxStoreMB:    st.ApproxStoreMB,
		LastWriteTS:      st.LastWriteTS,
		EventsDropsTotal: st.EventsDropsTotal,
		ReservedScopes:   st.ReservedScopes,
	})
}

// handleScopeList serves /scopelist — the per-scope counterpart of
// /stats. /stats is store-wide aggregate (O(1)); /scopelist is
// per-scope detail with alphabetical sort and cursor pagination, so
// even a 100k-scope store can be walked in fixed-size pages without
// making the response proportional to scope count.
//
// Query parameters:
//   - prefix : optional, literal strings.HasPrefix filter on scope name
//     (per-tenant footprint visibility without fetch-and-filter on the client)
//   - after  : optional cursor; returns scopes with name > after (strict)
//   - limit  : page size; defaults to DefaultLimit, clamped to MaxLimit
//
// Sort order is alphabetical: scope names don't move once created,
// so the cursor stays stable under concurrent writes. The next-page
// cursor is just the last `scope` field of the response — no
// dedicated next_cursor field, since the client already has it.
//
// Read-bookkeeping (§8) is not bumped on /scopelist hits: it is
// observability, not a content read, and would otherwise corrupt
// eviction-candidate signals that addons compute from
// read_count_total deltas.
func (api *API) handleScopeList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}

	query := r.URL.Query()
	prefix := query.Get("prefix")
	after := query.Get("after")

	// Reuse the scope shape rules for both filter inputs: same byte cap,
	// same control-character ban, same whitespace check. Empty values are
	// legitimate (no filter, start from beginning) and skip validation.
	if prefix != "" {
		if err := checkKeyField("prefix", prefix, MaxScopeBytes); err != nil {
			badRequest(w, err.Error())
			return
		}
	}
	if after != "" {
		if err := checkKeyField("after", after, MaxScopeBytes); err != nil {
			badRequest(w, err.Error())
			return
		}
	}

	limit, err := normalizeLimit(query.Get("limit"))
	if err != nil {
		badRequest(w, err.Error())
		return
	}

	entries, truncated := api.store.scopeList(prefix, after, limit)
	api.writeScopeListResponse(w, ScopeListResponse{
		OK:        true,
		Hit:       len(entries) > 0,
		Count:     len(entries),
		Truncated: truncated,
		Scopes:    entries,
	})
}

// writeScopeListResponse mirrors writeItemsResponse for /scopelist:
// single-buffer manual JSON, no marshal+splice doubling. Each
// scopeListEntry has ten scalar fields with no payload bytes, so
// the per-row cost is small — but the 1000-entry × 100-byte-name
// envelope still benefits from skipping the splice copy that
// writeJSONWithMetaCap performs in marshalWithApproxSize.
//
// The `hit`/`count`/`truncated`/`scopes` ordering matches what the
// previous orderedFields path produced; `hit` is `count > 0`, the
// same semantic /head and /tail derive from `len(items) > 0`, so
// the list-return read family stays uniform on the wire.
func (api *API) writeScopeListResponse(w http.ResponseWriter, resp ScopeListResponse) {
	estCapacity := int64(192) + int64(len(resp.Scopes))*150
	for i := range resp.Scopes {
		estCapacity += int64(len(resp.Scopes[i].Scope))
	}
	buf := make([]byte, 0, estCapacity)

	buf = append(buf, `{"ok":true,"hit":`...)
	if resp.Hit {
		buf = append(buf, `true`...)
	} else {
		buf = append(buf, `false`...)
	}
	buf = append(buf, `,"count":`...)
	buf = strconv.AppendInt(buf, int64(resp.Count), 10)
	buf = append(buf, `,"truncated":`...)
	if resp.Truncated {
		buf = append(buf, `true`...)
	} else {
		buf = append(buf, `false`...)
	}
	buf = append(buf, `,"scopes":[`...)
	for i := range resp.Scopes {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = appendScopeListEntryJSON(buf, resp.Scopes[i])
	}
	buf = append(buf, ']')

	estTotal := len(buf) + 30
	mbVal := float64(estTotal) / 1048576.0
	buf = append(buf, `,"approx_response_mb":`...)
	buf = strconv.AppendFloat(buf, mbVal, 'f', 4, 64)
	buf = append(buf, '}', '\n')

	if int64(len(buf)) > api.maxResponseBytes {
		responseTooLarge(w, int64(len(buf)), api.maxResponseBytes)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf)
}

// appendScopeListEntryJSON appends a single scopeListEntry to buf
// as JSON. Field order matches the struct-tag order in store.go's
// scopeListEntry definition; ApproxScopeMB renders as %.4f exactly
// like MB.MarshalJSON does.
func appendScopeListEntryJSON(buf []byte, e scopeListEntry) []byte {
	buf = append(buf, `{"scope":`...)
	buf = AppendJSONString(buf, e.Scope)
	buf = append(buf, `,"item_count":`...)
	buf = strconv.AppendInt(buf, int64(e.ItemCount), 10)
	buf = append(buf, `,"last_seq":`...)
	buf = strconv.AppendUint(buf, e.LastSeq, 10)
	buf = append(buf, `,"first_uuid":`...)
	buf = AppendJSONString(buf, e.FirstUUID)
	buf = append(buf, `,"last_uuid":`...)
	buf = AppendJSONString(buf, e.LastUUID)
	buf = append(buf, `,"approx_scope_mb":`...)
	buf = strconv.AppendFloat(buf, float64(e.ApproxScopeMB)/1048576.0, 'f', 4, 64)
	buf = append(buf, `,"created_ts":`...)
	buf = strconv.AppendInt(buf, e.CreatedTS, 10)
	buf = append(buf, `,"last_write_ts":`...)
	buf = strconv.AppendInt(buf, e.LastWriteTS, 10)
	buf = append(buf, `,"last_access_ts":`...)
	buf = strconv.AppendInt(buf, e.LastAccessTS, 10)
	buf = append(buf, `,"read_count_total":`...)
	buf = strconv.AppendUint(buf, e.ReadCountTotal, 10)
	return append(buf, '}')
}

func (api *API) handleHelp(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusMethodNotAllowed)
		_, _ = w.Write([]byte("method not allowed\n"))
		return
	}

	// /help intentionally links to the canonical RFC rather than
	// duplicating the spec in code: a stale long-form here would
	// drift out of sync with the source of truth.
	helpText := "scopecache — see instructions at https://github.com/VeloxCoding/scopecache/blob/main/docs/scopecache-core-rfc.md\n"

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(helpText))
}
