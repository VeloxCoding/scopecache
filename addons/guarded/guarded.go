// Package guarded is a scopecache addon: a token-gated /tail
// endpoint that confines a token-holder to scopes prefixed with their
// capability ID.
//
// Boundary contract:
//
//   - Imports `scopecache` (the public *Gateway) and stdlib only. No
//     reach into core internals; the addon is a pure consumer of the
//     public Go-API.
//   - Adapters mount the addon by calling RegisterRoutes(mux, gw)
//     after their own core RegisterRoutes pass. The addon ships with
//     the standard package; both the standalone binary
//     (cmd/scopecache) and the Caddy module (caddymodule) wire it in
//     unconditionally.
//
// Access model:
//
//   - GET /guarded-tail?scope=<s>[&limit=N&offset=M]
//   - Authorization: Bearer <token> is required.
//   - capID = base64url(sha256(token)) — 43 chars, full 256-bit hash,
//     URL-safe and shorter than hex. The plaintext token never enters
//     the cache; only its hash does.
//   - Lookup by id=capID in the operator-managed scope `tokensScope`
//     (`_tokens`). Missing header / unknown capID → 401, no further
//     work, no side effects.
//   - On hit, the request is dispatched to gw.Tail(capID + ":" + scope,
//     limit, offset). The capID prefix is stripped from items.scope on
//     the response so the client sees only its own scope name and
//     never its own hash on the wire.
//
// Operator workflow:
//
//   - Issue a token: /upsert into `_tokens` with id=capID and an opaque
//     payload (e.g. {} or audit metadata). The payload is never
//     inspected by the addon.
//   - Revoke a token: /delete on (`_tokens`, capID).
//   - The addon never writes to `_tokens` — it is a read-only access gate.
package guarded

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/VeloxCoding/scopecache"
)

// tokensScope is the cache-internal scope where the addon looks up
// capIDs. Naming convention: leading underscore flags it as
// infrastructure. The core makes no special promise about `_tokens`;
// the addon owns the invariant.
const tokensScope = "_tokens"

// RegisterRoutes mounts the addon's routes on mux. Adapters call this
// after their own core RegisterRoutes pass. Idempotent registration
// is the caller's responsibility (http.ServeMux panics on duplicate
// patterns); each adapter calls it exactly once per *Gateway.
func RegisterRoutes(mux *http.ServeMux, gw *scopecache.Gateway) {
	h := &handler{gw: gw}
	mux.HandleFunc("/guarded-tail", h.handleTail)
}

type handler struct {
	gw *scopecache.Gateway
}

// capID returns base64url(sha256(token)) — 43 chars, no padding,
// 256-bit collision resistance. The plaintext token is never stored.
func capID(token string) string {
	sum := sha256.Sum256([]byte(token))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// extractBearer reads the Authorization header and returns the bearer
// token, or "" when the header is missing, not a Bearer scheme, or
// the token portion is empty after trimming.
func extractBearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}

// handleTail is the GET /guarded-tail handler.
//
// Order: method → access check → query parse → dispatch → strip-prefix → respond.
// The access check runs before query parse so a caller without a
// valid token does no shape-validation work and learns nothing about
// the addon's wire shape beyond what is publicly documented.
func (h *handler) handleTail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		errorJSON(w, http.StatusMethodNotAllowed, "the HTTP method is not allowed for this endpoint")
		return
	}

	token := extractBearer(r)
	if token == "" {
		errorJSON(w, http.StatusUnauthorized, "missing or malformed Authorization header")
		return
	}

	cid := capID(token)
	if _, hit := h.gw.GetByID(tokensScope, cid); !hit {
		errorJSON(w, http.StatusUnauthorized, "invalid or unknown token")
		return
	}

	scope, limit, offset, err := parseTailQuery(r)
	if err != nil {
		errorJSON(w, http.StatusBadRequest, err.Error())
		return
	}

	prefix := cid + ":"
	items, hasMore, found := h.gw.Tail(prefix+scope, limit, offset)

	// Strip the capID prefix so the client never sees its own hash on
	// the wire. A returned item.scope without the prefix would mean an
	// invariant violation upstream — leave the value alone in that
	// case rather than corrupt the wire response.
	for i := range items {
		if strings.HasPrefix(items[i].Scope, prefix) {
			items[i].Scope = items[i].Scope[len(prefix):]
		}
	}

	body := map[string]any{
		"ok":        true,
		"hit":       found && len(items) > 0,
		"count":     len(items),
		"offset":    offset,
		"truncated": hasMore,
		"items":     items,
	}
	if items == nil {
		body["items"] = []scopecache.Item{}
	}
	writeJSON(w, http.StatusOK, body)
}

// parseTailQuery extracts scope/limit/offset from the URL query and
// applies the same default/clamp rules core /tail uses. limit
// defaults to scopecache.DefaultLimit, clamps at scopecache.MaxLimit;
// offset defaults to 0.
func parseTailQuery(r *http.Request) (scope string, limit, offset int, err error) {
	q := r.URL.Query()
	scope = q.Get("scope")
	if scope == "" {
		return "", 0, 0, errors.New("the 'scope' query parameter is required")
	}

	limit = scopecache.DefaultLimit
	if raw := q.Get("limit"); raw != "" {
		n, parseErr := strconv.Atoi(raw)
		if parseErr != nil || n <= 0 {
			return "", 0, 0, errors.New("the 'limit' parameter must be a positive integer")
		}
		if n > scopecache.MaxLimit {
			n = scopecache.MaxLimit
		}
		limit = n
	}

	if raw := q.Get("offset"); raw != "" {
		n, parseErr := strconv.Atoi(raw)
		if parseErr != nil || n < 0 {
			return "", 0, 0, errors.New("the 'offset' parameter must be a non-negative integer")
		}
		offset = n
	}
	return scope, limit, offset, nil
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	out, err := json.Marshal(body)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"ok":false,"error":"the response failed to marshal"}` + "\n"))
		return
	}
	w.WriteHeader(code)
	_, _ = w.Write(out)
	_, _ = w.Write([]byte("\n"))
}

func errorJSON(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"ok": false, "error": msg})
}
