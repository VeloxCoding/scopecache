# FrankenPHP-extension tooling

All build / validate / bench tooling for the scopecache FrankenPHP
extension — the extension itself (the Go source) lives in
[`addons/frankenphp-ext/`](../../addons/frankenphp-ext/).

## Quick reference

```bash
./build.sh           # compile dist/frankenphp (~1-3 min warm)
./smoke.sh           # post-build sanity (~5 s, 8 checks)
./validate.sh        # full correctness suite (~5-10 s, ~170 checks)
./bench.sh           # per-call latency + throughput (get/tail/append @ 54 B)
./bench.sh --sweep   # scopecache_get cost across payload sizes
```

## What lives here

| file | role |
|---|---|
| [`Dockerfile.gen`](Dockerfile.gen) | Cached generator image — PHP-ZTS headers + `frankenphp-gen` (built from FrankenPHP master so `extension-init` is present) + `xcaddy` + the right `GEN_STUB_SCRIPT` env path. ~3 min cold, instant warm. |
| [`Dockerfile.bench`](Dockerfile.bench) | Runtime image adding `phpredis` + `memcached` C-extensions to stock FrankenPHP — reserved for future Redis/Memcached comparison runs. |
| [`build.sh`](build.sh) | Two-stage build orchestrator: builds the cached image (stage 1), runs it with the addon source + scopecache core bind-mounted to produce `dist/frankenphp` (stage 2). |
| [`smoke.sh`](smoke.sh) + [`test.php`](test.php) | Post-build sanity — boots the binary, exercises a few cgo calls, asserts the shared `*Gateway` is reachable from PHP. |
| [`validate.sh`](validate.sh) + [`validate.php`](validate.php) | Full correctness suite covering all 19 //export_php functions: envelope-shape checks, payload-decode round-trips, error envelopes, byte-exact warm/rebuild, etc. |
| [`bench.sh`](bench.sh) + [`bench.php`](bench.php) | Per-call latency + throughput for the cgo hot path (`scopecache_get` / `_tail` / `_append`). `bench.sh --sweep` reuses the same PHP to chart `_get` cost across payload sizes 54 B → 10 KiB. |
| [`Caddyfile.bench`](Caddyfile.bench) | Runtime config used by every script above. Exposes both paths in one process: scopecache as a Caddy module on `:8080` plus `php_server` for the bench PHP files. |
| `dist/` (gitignored) | Build output — `dist/frankenphp` is a FrankenPHP binary with the scopecache extension baked in. |

## Build-chain pitfalls

These are the gotchas every sed-patch in `build.sh` is working around.
Each survived a build session that fell over on it.

### 1. `// export_php:` (with space) on disk, `//export_php:` (tight) at build time

`frankenphp-gen extension-init` only matches the **tight** form
`//export_php:` (no space after `//`). `gofmt` and most editor
"format-on-save" rules rewrite that into `// export_php:` (with space)
because it would otherwise look like an unparseable comment.

**Workaround:** keep `// export_php:` on disk (gofmt-clean), `sed` it
back to `//export_php:` inside the build container before invoking the
generator.

### 2. `RETURN_EMPTY_STRING` / `RETURN_EMPTY_ARRAY` instead of `RETURN_NULL`

The upstream extgen template emits `RETURN_EMPTY_STRING()` /
`RETURN_EMPTY_ARRAY()` when the Go function returns `nil`, regardless
of whether the directive declared the return type as nullable
(`?string` / `?array`). That collapses PHP `null` into `""` / `[]`,
breaking idiomatic `if ($r === null)` miss checks.

**Workaround:** post-process the generated C:

```bash
sed -i -e 's|RETURN_EMPTY_STRING();|RETURN_NULL();|g' \
       -e 's|RETURN_EMPTY_ARRAY();|RETURN_NULL();|g' \
    /work/ext/build/*.c /work/ext/*.c
```

### 3. Apostrophes inside the outer `bash -c '...'`

`docker run ... bash -c '...'` uses single quotes outside. A single
apostrophe in any of the heredoc-style comments closes the outer
string mid-script — symptoms range from "permission denied" to
"unexpected token". Keep `bash -c` body text apostrophe-free.

### 4. Generator wants `int64`, not `C.zend_long`

PHP `int` parameters must surface as Go `int64` in the function
signature. `C.zend_long` silently makes the generator skip the
function (no error; the symbol just never reaches PHP). Always
declare PHP-`int` params as Go `int64`.

### 5. cgo macros need static-inline trampolines

PHP's `ZVAL_*` and `zend_new_array` macros cannot be invoked through
cgo (cgo only sees functions). Each macro you want from Go needs a
small static-inline C wrapper in the cgo preamble:

```c
static inline void sc_zval_str(zval *zv, zend_string *s) { ZVAL_STR(zv, s); }
static inline zend_array *sc_zend_new_array(uint32_t size) { return zend_new_array(size); }
```

### 6. `MSYS_NO_PATHCONV=1` on Windows / Git-Bash hosts

Git-Bash on Windows rewrites absolute Linux-style paths like
`/scopecache` into Windows drive paths (`C:\scopecache`) before they
reach the docker daemon. Prefix every `docker build` and `docker run`
with `MSYS_NO_PATHCONV=1`.

### 7. xcaddy build flags for cgo

```bash
CGO_ENABLED=1 \
XCADDY_GO_BUILD_FLAGS="-ldflags=-linkmode=external" \
CGO_CFLAGS="-D_GNU_SOURCE $(php-config --includes)" \
CGO_LDFLAGS="$(php-config --ldflags) $(php-config --libs)" \
    xcaddy build ...
```

- `-D_GNU_SOURCE` — PHP-Zend headers reference `memrchr()` (a GNU extension).
- `-linkmode=external` — Go 1.26's internal linker chokes when
  stitching multiple cgo packages (FrankenPHP + the extension) into
  one binary.

### 8. UAF on string map-keys: write paths must copy

`unsafe.String((*byte)(unsafe.Pointer(&s.val)), int(s.len))` produces a
zero-copy alias over PHP's emalloc'd `zend_string` bytes. That's
safe for **synchronous read paths** (the cache uses the key, returns,
PHP frees the zend_string at request end). It is **not** safe when
the string becomes a permanent map key — those aliases point at
freed PHP memory after the request ends, indexing the map by garbage.

For write paths that retain keys, deep-copy via `C.GoStringN` instead:

```go
func zendStringCopy(s *C.zend_string) string {
    return C.GoStringN((*C.char)(unsafe.Pointer(&s.val)), C.int(s.len))
}
```

The scopecache extension does this distinction: `scopecache_get` uses
the alias, `scopecache_append` uses the copy.

## Versioning

Both Dockerfiles pin `dunglas/frankenphp:1.12-*`. Bump the tag here
when you want a newer FrankenPHP / PHP / Go combination, then
`./build.sh --rebuild-gen-image`.
