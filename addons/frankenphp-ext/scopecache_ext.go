// scopecache_ext.go — FrankenPHP extension that exposes scopecache's
// Go-level *Gateway directly to PHP.
//
// Why this exists: even when scopecache runs inside the same
// FrankenPHP/Caddy binary as the PHP app, PHP→cache calls today still
// go through HTTP (libcurl → loopback TCP → Caddy routing → scopecache
// handler → JSON encode/decode on both sides). The harness measured
// that loopback-HTTP floor at ~3.5 ms for an 11-17 µs cache lookup —
// a ~200× overhead ratio dominated entirely by transport.
//
// This extension removes that transport layer. PHP calls a function
// (scopecache_get, etc.); the FrankenPHP extension generator turns
// that into a C-level PHP_FUNCTION whose body lands in this Go file;
// the Go function calls *Gateway methods in the same process; the
// return value crosses back through cgo as a native PHP value.
//
// Measured wins (54-byte payload, FrankenPHP 8.5 ZTS):
//   - ~750× vs PHP→scopecache over loopback HTTP (persistent curl handle)
//   - ~400× vs PHP→phpredis→Redis (persistent connection)
//   - ~1900× vs PHP→phpredis→Redis (fresh connection per call)
// See addons/frankenphp-ext/bench.php for the measurement harness.
//
// Boundary discipline (CLAUDE.md):
//   - This is an addon. The core never changes shape to accommodate it.
//   - PHP and the Caddy module share one *Gateway via the process-wide
//     named registry in gateway_registry.go: the caddymodule registers
//     under "default" during Provision(), this extension looks it up at
//     every call. Same data both ways; same Caddyfile config; same
//     /stats output. No second hidden cache.

package scopecache_ext

// #include <Zend/zend_types.h>
import "C"

import (
	"unsafe"

	"github.com/VeloxCoding/scopecache"
	"github.com/dunglas/frankenphp"
)

// scopecache_get looks up a single item by scope + id and returns its
// payload bytes as a PHP string. Misses return null.
//
// Wire shape (PHP-visible):
//
//	scopecache_get(string $scope, string $id): ?string
//
// Returned bytes are the verbatim JSON payload — the same bytes
// served by GET /get?scope=X&id=Y under the "item.payload" key in
// the HTTP envelope, just without the envelope.
//
// Empty/whitespace inputs and over-cap scope/id strings are not
// validated here; the *Gateway layer enforces shape rules and a
// future revision will surface validation errors as PHP exceptions.
// For the prototype, invalid input simply returns null.
//
// Also returns null when no scopecache caddymodule is loaded in this
// binary (LookupGateway returns nil). An operator seeing only nulls
// should check that the Caddyfile has a `scopecache {}` block — without
// it, no Provision() ever ran, so no *Gateway is registered.
//
// export_php:function scopecache_get(string $scope, string $id): ?string
func scopecache_get(scope *C.zend_string, id *C.zend_string) unsafe.Pointer {
	gw := scopecache.LookupGateway("default")
	if gw == nil {
		return nil
	}
	scopeStr := frankenphp.GoString(unsafe.Pointer(scope))
	idStr := frankenphp.GoString(unsafe.Pointer(id))
	item, found := gw.GetByID(scopeStr, idStr)
	if !found {
		return nil
	}
	return frankenphp.PHPString(string(item.Payload), false)
}
