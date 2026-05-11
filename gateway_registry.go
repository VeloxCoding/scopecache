// gateway_registry.go — process-wide named *Gateway lookup for
// in-process consumers that don't otherwise have a handle on the
// gateway the Caddy module is serving from.
//
// Why this exists: when scopecache runs as a Caddy module, the
// adapter's Provision() creates a *Gateway and wires it into
// *API + the HTTP mux. PHP extensions, future Go-side addons, or
// any other in-process caller that wants to hit the *same* cache
// instance the HTTP routes serve cannot reach the Caddy adapter's
// gateway through ordinary import — the field is unexported and
// the adapter does not expose an accessor.
//
// The registry solves that without coupling the core to any
// specific adapter. The caddymodule registers its gateway under a
// conventional name during Provision() and deregisters during
// Cleanup(); consumers (typically a single hard-coded "default")
// look it up at use time.
//
// Lifecycle on Caddy config reload (worth understanding):
//
//   1. Old instance running, gw_old registered under "default".
//   2. New Provision() runs first, overwrites the registration
//      with gw_new under "default".
//   3. Caddy switches traffic to the new instance.
//   4. Old Cleanup() runs.
//
// If step 4 blindly called RegisterGateway("default", nil) it would
// clobber the new instance's registration. DeregisterGatewayIf
// guards against this: it removes the entry only if it still points
// at the caller's own gateway pointer. The caddymodule's Cleanup
// uses DeregisterGatewayIf, so a normal reload keeps the new
// registration intact while a clean shutdown still removes the
// dangling pointer.
//
// In-flight cgo or Go calls holding a pointer obtained from a
// previous LookupGateway stay valid: Go's GC will not free a
// *Gateway anyone still references. Subsequent LookupGateway calls
// pick up the new registration.

package scopecache

import "sync"

var (
	registryMu sync.RWMutex
	registry   = map[string]*Gateway{}
)

// RegisterGateway publishes gw under name. Pass a nil gw to remove
// the registration unconditionally — use DeregisterGatewayIf instead
// from Cleanup-style paths that must avoid clobbering a newer
// registration during config reload.
func RegisterGateway(name string, gw *Gateway) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if gw == nil {
		delete(registry, name)
		return
	}
	registry[name] = gw
}

// LookupGateway returns the *Gateway registered under name, or nil
// if nothing is registered there. Callers MUST handle nil — a
// consumer running in a binary without the scopecache caddymodule,
// or before Provision has completed, will see nil.
func LookupGateway(name string) *Gateway {
	registryMu.RLock()
	defer registryMu.RUnlock()
	return registry[name]
}

// DeregisterGatewayIf removes the registration under name only if
// the currently-registered Gateway is the exact gw passed in. This
// is the safe deregister for caddymodule Cleanup: it preserves a
// newer Provision's registration during reload while still cleaning
// up the entry when the module is fully removed.
func DeregisterGatewayIf(name string, gw *Gateway) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if registry[name] == gw {
		delete(registry, name)
	}
}
