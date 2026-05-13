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
//
// See addons/frankenphp-ext/bench.php for the measurement harness.
//
// Boundary discipline (CLAUDE.md):
//   - This is an addon. The core never changes shape to accommodate it.
//   - PHP and the Caddy module share one *Gateway via the process-wide
//     named registry in gateway_registry.go: the caddymodule registers
//     under "default" during Provision(), this extension looks it up at
//     every call. Same data both ways; same Caddyfile config; same
//     /stats output. No second hidden cache.
//
// Hot-path discipline:
//   - The Gateway pointer is looked up once at package init via
//     scopecache.LookupGatewaySlot, then read per call with a single
//     atomic Load (~1-2 ns) instead of going through the registry's
//     RLock + map lookup (~30-40 ns). Caddy reload swaps the slot
//     contents atomically — no invalidation logic needed here.
//   - Scope/id strings cross C→Go as zero-copy unsafe.String views into
//     the zend_string's emalloc'd byte storage. Safe because
//     scopecache.GetByID consumes them synchronously and retains no
//     references (see store.get and buffer_read.getByID). If that
//     contract ever changes, switch back to frankenphp.GoString.
//   - The returned payload skips the []byte→string→zend_string detour
//     that frankenphp.PHPString takes, directly emalloc'ing a fresh
//     zend_string in the PHP request arena via phpStringFromBytes.

package scopecache_ext

// #include <Zend/zend_string.h>
import "C"

import (
	"sync/atomic"
	"unsafe"

	"github.com/VeloxCoding/scopecache"
)

// defaultSlot is the atomic *Gateway slot for the "default" name,
// cached at package init. LookupGatewaySlot lazily creates the slot
// on first reference, so this is safe even if package init runs
// before the caddymodule's Provision() — the slot holds nil until
// Register fires, and our scopecache_get already handles that case.
var defaultSlot *atomic.Pointer[scopecache.Gateway] = scopecache.LookupGatewaySlot("default")

// zendStringView returns a Go string aliasing the zend_string's byte
// storage — zero copies, zero allocations. Valid only for the
// duration of the calling PHP_FUNCTION: the underlying zend_string is
// PHP's emalloc'd request memory and lives at least as long as the
// request, but callers must NOT retain the returned string past their
// own cgo-call return.
//
// scopecache.GetByID satisfies this constraint — scope/id are used as
// synchronous map keys and never stored. Cross-checked in
// [store.go](../../store.go) and [buffer_read.go](../../buffer_read.go).
func zendStringView(s *C.zend_string) string {
	if s == nil {
		return ""
	}
	return unsafe.String((*byte)(unsafe.Pointer(&s.val)), int(s.len))
}

// phpStringFromBytes emalloc's a fresh zend_string in the PHP request
// arena and copies the given bytes in, skipping the []byte→string→
// zend_string detour frankenphp.PHPString takes. The returned pointer
// is owned by the wrapper's RETURN_STR — PHP frees it on request
// shutdown.
//
// Empty input returns nil; combined with our build-time wrapper
// patch (RETURN_EMPTY_STRING→RETURN_NULL), PHP sees null. NB: this
// means an item legitimately holding a 0-byte payload would be
// reported as a miss. Safe today because scopecache validation
// requires non-empty JSON; documented here so a future validation
// loosening upstream doesn't silently break this extension.
func phpStringFromBytes(b []byte) unsafe.Pointer {
	if len(b) == 0 {
		return nil
	}
	return unsafe.Pointer(C.zend_string_init(
		(*C.char)(unsafe.Pointer(&b[0])),
		C.size_t(len(b)),
		C._Bool(false),
	))
}

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
// binary (defaultSlot.Load() returns nil because no Provision ever
// stored a Gateway into it). An operator seeing only nulls should
// check that the Caddyfile has a `scopecache {}` block — without it,
// no Provision() ever ran, so no *Gateway is registered.
//
// export_php:function scopecache_get(string $scope, string $id): ?string
func scopecache_get(scope *C.zend_string, id *C.zend_string) unsafe.Pointer {
	gw := defaultSlot.Load()
	if gw == nil {
		return nil
	}
	item, found := gw.GetByID(zendStringView(scope), zendStringView(id))
	if !found {
		return nil
	}
	return phpStringFromBytes(item.Payload)
}
