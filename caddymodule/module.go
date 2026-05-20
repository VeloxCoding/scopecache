// Package caddymodule exposes scopecache as a Caddy HTTP handler.
//
// The core scopecache package stays stdlib-only (no Caddy imports)
// and owns every cache semantic. This adapter:
//
//   - translates Caddy's JSON / Caddyfile config into scopecache.Config,
//   - wires the core's route table onto an internal http.ServeMux, and
//   - delegates each matched request to that mux from ServeHTTP.
//
// Cross-cutting concerns that require request context — auth,
// identity, per-tenant scope-prefix rewrites, rate-limit hooks —
// belong in this adapter layer or in addon sub-packages built on
// top of the public *scopecache.API surface, not in core.
package caddymodule

import (
	"context"
	"fmt"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/VeloxCoding/scopecache"
	"github.com/VeloxCoding/scopecache/addons/guarded"
	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

// Handler is the Caddy HTTP handler that embeds scopecache.
//
// JSON config fields map 1:1 to the same capacity knobs the standalone
// binary reads from env vars (SCOPECACHE_SCOPE_MAX_ITEMS,
// SCOPECACHE_MAX_STORE_MB, SCOPECACHE_MAX_ITEM_MB). Zero / empty
// values fall back to the compile-time defaults declared in the core
// package.
type Handler struct {
	// ScopeMaxItems caps items per scope. 0 = use scopecache.ScopeMaxItems.
	ScopeMaxItems int `json:"scope_max_items,omitempty"`
	// MaxStoreMB caps aggregate store size in MiB. 0 = use scopecache.MaxStoreMiB.
	MaxStoreMB int `json:"max_store_mb,omitempty"`
	// MaxItemMB caps a single item's approxItemSize in MiB. 0 = use scopecache.MaxItemBytes.
	MaxItemMB int `json:"max_item_mb,omitempty"`
	// InitCommand is the absolute path to an executable invoked once
	// at Provision time, before Caddy starts listening. The script
	// reaches the cache via `curl --unix-socket "$SCOPECACHE_SOCKET_PATH"`
	// against a per-instance private socket the adapter binds for
	// the duration of init. Empty (default) = no init script. Full
	// contract on scopecache.Gateway.RunInitCommand and RFC §2.7.
	InitCommand string `json:"init_command,omitempty"`
	// InitTimeoutSec caps how long the init command may run before
	// the helper SIGKILLs its process group. 0 (default) = no
	// timeout; Caddy reload / shutdown still cancels the script.
	InitTimeoutSec int `json:"init_timeout_sec,omitempty"`

	api *scopecache.API
	mux *http.ServeMux
	// gateway holds the *Gateway this Provision created, so Cleanup
	// can pass it to DeregisterGatewayIf and not clobber a newer
	// instance's registration during Caddy config reload.
	gateway *scopecache.Gateway
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
// internal mux. Called once per module instance at Caddy start / config
// reload. Zero-valued numeric directives fall back to the core's
// compile-time defaults via Config.WithDefaults inside NewStore.
func (h *Handler) Provision(ctx caddy.Context) error {
	if err := h.validateConfig(); err != nil {
		return err
	}
	gw := scopecache.NewGateway(scopecache.Config{
		ScopeMaxItems: h.ScopeMaxItems,
		MaxStoreBytes: int64(h.MaxStoreMB) << 20,
		MaxItemBytes:  int64(h.MaxItemMB) << 20,
	})
	h.api = scopecache.NewAPI(gw, scopecache.APIConfig{})
	h.mux = http.NewServeMux()
	h.api.RegisterRoutes(h.mux)
	guarded.RegisterRoutes(h.mux, gw)

	// Run init at Provision time. A failure is logged and ignored
	// (empty cache is still a working cache).
	if h.InitCommand != "" {
		if err := h.runInitWithPrivateSocket(ctx, gw); err != nil {
			caddy.Log().Named("scopecache.init").Sugar().Warnf("init: %v", err)
		}
	}

	// Publish the gateway under the conventional "default" name so
	// in-process consumers (PHP extensions, future Go-side addons)
	// can hit the same cache instance the HTTP routes serve. See
	// gateway_registry.go in the core for lifecycle rationale.
	h.gateway = gw
	scopecache.RegisterGateway("default", gw)
	return nil
}

// runInitWithPrivateSocket binds a per-instance temp Unix socket
// (0o700 dir from MkdirTemp), runs the init command with
// SCOPECACHE_SOCKET_PATH pointing at it, then tears the socket
// down. ctx cancellation kills a hung script's whole process group;
// h.InitTimeoutSec > 0 wraps ctx with a hard deadline.
func (h *Handler) runInitWithPrivateSocket(ctx context.Context, gw *scopecache.Gateway) error {
	logf := caddy.Log().Named("scopecache.init").Sugar().Infof
	logger := caddy.Log().Named("scopecache.init").Sugar()

	dir, err := os.MkdirTemp("", "scopecache-init-")
	if err != nil {
		return fmt.Errorf("create temp dir for init socket: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(dir); err != nil {
			logger.Warnf("remove temp dir %s: %v", dir, err)
		}
	}()

	sockPath := filepath.Join(dir, "init.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		return fmt.Errorf("listen on init socket %s: %w", sockPath, err)
	}

	server := &http.Server{Handler: h.mux}
	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		_ = server.Serve(ln)
	}()

	runCtx := ctx
	if h.InitTimeoutSec > 0 {
		var cancelTimeout context.CancelFunc
		runCtx, cancelTimeout = context.WithTimeout(ctx, time.Duration(h.InitTimeoutSec)*time.Second)
		defer cancelTimeout()
	}

	runErr := gw.RunInitCommand(
		runCtx,
		h.InitCommand,
		[]string{"SCOPECACHE_SOCKET_PATH=" + sockPath},
		logf,
	)

	// Tear the socket down before Provision returns. Shutdown
	// gracefully closes idle connections; in-flight curls from the
	// init script have already completed (RunInitCommand is sync).
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Warnf("shutdown init socket: %v", err)
	}
	<-serveDone

	return runErr
}

// Cleanup is called by Caddy when the module is being torn down (config
// reload, server shutdown).
func (h *Handler) Cleanup() error {
	// Deregister from the gateway registry. DeregisterGatewayIf is
	// the safe form: during Caddy config reload the NEW Provision
	// has already overwritten our entry by the time we run, and we
	// must not clobber it. The conditional check matches only if our
	// own gateway pointer is still the active registration.
	if h.gateway != nil {
		scopecache.DeregisterGatewayIf("default", h.gateway)
		h.gateway = nil
	}
	return nil
}

// ServeHTTP dispatches to the scopecache mux. Any path the mux does not
// recognise falls through to the next Caddy handler — this lets operators
// mount scopecache under a path prefix (`handle /cache/*`) alongside other
// handlers without scopecache swallowing unrelated traffic.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	if handler, pattern := h.mux.Handler(r); pattern != "" {
		handler.ServeHTTP(w, r)
		return nil
	}
	return next.ServeHTTP(w, r)
}

// UnmarshalCaddyfile parses the `scopecache` handler directive. All
// subdirectives are optional; an unset value falls back to the core default
// inside Provision. Example:
//
//	scopecache {
//	    scope_max_items   100000
//	    max_store_mb      100
//	    max_item_mb       1
//	    init_command      /usr/local/bin/rebuild.sh
//	    init_timeout_sec  600
//	}
func (h *Handler) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	for d.Next() {
		if d.NextArg() {
			return d.ArgErr()
		}
		for d.NextBlock(0) {
			key := d.Val()
			if !d.NextArg() {
				return d.ArgErr()
			}
			value := d.Val()

			if key == "init_command" {
				h.InitCommand = value
				continue
			}

			n, err := strconv.Atoi(value)
			if err != nil {
				return d.Errf("%s: %v", key, err)
			}
			switch key {
			case "scope_max_items":
				h.ScopeMaxItems = n
			case "max_store_mb":
				h.MaxStoreMB = n
			case "max_item_mb":
				h.MaxItemMB = n
			case "init_timeout_sec":
				h.InitTimeoutSec = n
			default:
				return d.Errf("unrecognized option: %s", key)
			}
		}
	}
	return nil
}

// validateConfig rejects values the standalone binary's env-var
// parsers would have ignored with a warning (negative integers).
//
// maxConfigMB / maxConfigSec are the upper bounds beyond which a
// later unit conversion in Provision would silently overflow int64:
//
//   - MaxStoreMB / MaxItemMB:    `int64(value) << 20` (MiB→bytes).
//   - InitTimeoutSec:            `time.Duration(value) * time.Second`
//     (seconds → nanoseconds).
//
// These values are not practical, but rejecting them prevents a
// silently-wrong cap.
const (
	maxConfigMB  = math.MaxInt64 >> 20                // ~8.79 trillion MiB
	maxConfigSec = math.MaxInt64 / int64(time.Second) // ~292 years
)

func (h *Handler) validateConfig() error {
	for _, e := range []struct {
		key      string
		value    int64
		upper    int64
		upperFmt string
	}{
		// scope_max_items is a plain int count — no unit conversion,
		// no upper bound check beyond non-negative. upper == 0
		// disables the bound check for that row.
		{"scope_max_items", int64(h.ScopeMaxItems), 0, ""},
		{"max_store_mb", int64(h.MaxStoreMB), maxConfigMB, "MiB"},
		{"max_item_mb", int64(h.MaxItemMB), maxConfigMB, "MiB"},
		{"init_timeout_sec", int64(h.InitTimeoutSec), maxConfigSec, "seconds"},
	} {
		if e.value < 0 {
			return fmt.Errorf("%s must be zero or a positive integer (got %d); 0 falls back to the compile-time default", e.key, e.value)
		}
		if e.upper > 0 && e.value > e.upper {
			return fmt.Errorf("%s=%d exceeds the maximum (%d %s); larger values would overflow int64 after unit conversion",
				e.key, e.value, e.upper, e.upperFmt)
		}
	}
	return nil
}

// parseCaddyfile is the Caddyfile-syntax entry point registered with
// http.handlers so `scopecache { ... }` is recognised as a handler directive.
func parseCaddyfile(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	var m Handler
	if err := m.UnmarshalCaddyfile(h.Dispenser); err != nil {
		return nil, err
	}
	return &m, nil
}

func init() {
	caddy.RegisterModule(Handler{})
	httpcaddyfile.RegisterHandlerDirective("scopecache", parseCaddyfile)
	// Without an explicit order Caddy rejects the Caddyfile directive with
	// "not an ordered HTTP handler". Placing scopecache just before the
	// `respond` catch-all matches how terminal handlers are usually slotted
	// in and means operators never need to write a manual `order` line.
	httpcaddyfile.RegisterDirectiveOrder("scopecache", httpcaddyfile.Before, "respond")
}

var (
	_ caddy.Module                = (*Handler)(nil)
	_ caddy.Provisioner           = (*Handler)(nil)
	_ caddy.CleanerUpper          = (*Handler)(nil)
	_ caddyhttp.MiddlewareHandler = (*Handler)(nil)
	_ caddyfile.Unmarshaler       = (*Handler)(nil)
)
