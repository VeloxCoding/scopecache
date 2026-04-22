// Package caddymodule exposes scopecache as a Caddy HTTP handler module.
//
// The core scopecache package stays stdlib-only (no Caddy imports) and owns
// every cache semantic. This adapter:
//
//   - translates Caddy's JSON config into scopecache.Config,
//   - wires the core's route table onto an internal http.ServeMux, and
//   - delegates each matched request to that mux from ServeHTTP.
//
// Cross-cutting concerns that require request context — auth, identity,
// per-tenant scope-prefix enforcement, rate-limit hooks — belong in this
// adapter layer (see CLAUDE.md "boundary rule"). They are not wired up yet;
// this file is the skeleton they will hang off of.
package caddymodule

import (
	"net/http"

	"github.com/VeloxCoding/scopecache"
	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

// Handler is the Caddy HTTP handler that embeds scopecache.
//
// JSON config fields map 1:1 to the same capacity knobs the standalone
// binary reads from env vars (SCOPECACHE_SCOPE_MAX_ITEMS, SCOPECACHE_MAX_STORE_MB,
// SCOPECACHE_MAX_ITEM_MB). Zero values fall back to the compile-time defaults
// declared in the core package.
//
// MaxStoreMB and MaxItemMB are MiB-facing (matching the env-var convention);
// they are converted to bytes in Provision before being handed to the core.
type Handler struct {
	// ScopeMaxItems caps items per scope. 0 = use scopecache.ScopeMaxItems.
	ScopeMaxItems int `json:"scope_max_items,omitempty"`
	// MaxStoreMB caps aggregate store size in MiB. 0 = use scopecache.MaxStoreMiB.
	MaxStoreMB int `json:"max_store_mb,omitempty"`
	// MaxItemMB caps a single item's approxItemSize in MiB. 0 = use scopecache.MaxItemBytes.
	MaxItemMB int `json:"max_item_mb,omitempty"`

	api *scopecache.API
	mux *http.ServeMux
}

// CaddyModule returns the Caddy module registration. The ID places this under
// http.handlers.* so it can be used as a `handle` directive in a Caddyfile
// or as a JSON handler entry.
func (Handler) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.scopecache",
		New: func() caddy.Module { return new(Handler) },
	}
}

// Provision builds the core Store + API and registers its routes on an
// internal mux. Called once per module instance at Caddy start / config reload.
func (h *Handler) Provision(_ caddy.Context) error {
	cfg := scopecache.Config{
		ScopeMaxItems: h.ScopeMaxItems,
		MaxStoreBytes: int64(h.MaxStoreMB) << 20,
		MaxItemBytes:  int64(h.MaxItemMB) << 20,
	}
	if cfg.ScopeMaxItems == 0 {
		cfg.ScopeMaxItems = scopecache.ScopeMaxItems
	}
	if cfg.MaxStoreBytes == 0 {
		cfg.MaxStoreBytes = int64(scopecache.MaxStoreMiB) << 20
	}
	if cfg.MaxItemBytes == 0 {
		cfg.MaxItemBytes = int64(scopecache.MaxItemBytes)
	}

	store := scopecache.NewStore(cfg)
	h.api = scopecache.NewAPI(store)
	h.mux = http.NewServeMux()
	h.api.RegisterRoutes(h.mux)
	return nil
}

// ServeHTTP dispatches to the scopecache mux. Any path the mux does not
// recognise falls through to the next Caddy handler — this lets operators
// mount scopecache under a path prefix (`handle /cache/*`) alongside other
// handlers without scopecache swallowing unrelated traffic.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	if _, pattern := h.mux.Handler(r); pattern != "" {
		h.mux.ServeHTTP(w, r)
		return nil
	}
	return next.ServeHTTP(w, r)
}

func init() {
	caddy.RegisterModule(Handler{})
}

var (
	_ caddy.Module                = (*Handler)(nil)
	_ caddy.Provisioner           = (*Handler)(nil)
	_ caddyhttp.MiddlewareHandler = (*Handler)(nil)
)
