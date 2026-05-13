# scopecache as a FrankenPHP extension

Exposes scopecache's `*Gateway` directly to PHP — bypasses HTTP /
cURL / JSON encoding on the PHP→cache hop when scopecache and PHP
run together in the same FrankenPHP binary. PHP and the Caddy module
share **one** `*Gateway` through the process-wide registry in
[`gateway_registry.go`](../../gateway_registry.go); no second hidden
cache.

This directory holds **only** the addon source. Build / validate /
bench tooling lives in
[`tools/frankenphp-ext/`](../../tools/frankenphp-ext/).

## What's exposed

19 PHP functions, each mirroring an HTTP endpoint on `*Gateway`:

| PHP signature | Maps to |
|---|---|
| `scopecache_get(scope, id): ?array` | `GET /get` |
| `scopecache_get_by_seq(scope, seq): ?array` | `GET /get?seq=` |
| `scopecache_head(scope, after_seq, limit): ?array` | `GET /head` |
| `scopecache_tail(scope, limit): ?array` | `GET /tail` |
| `scopecache_render_by_id(scope, id): ?string` | `GET /render` (raw bytes) |
| `scopecache_render_by_seq(scope, seq): ?string` | `GET /render?seq=` (raw bytes) |
| `scopecache_append(scope, id, payload): ?array` | `POST /append` |
| `scopecache_upsert(scope, id, payload): ?array` | `POST /upsert` |
| `scopecache_update(scope, id, payload): ?array` | `POST /update` |
| `scopecache_counter_add(scope, id, by): ?array` | `POST /counter_add` |
| `scopecache_delete(scope, id): ?array` | `POST /delete` |
| `scopecache_delete_by_seq(scope, seq): ?array` | `POST /delete?seq=` |
| `scopecache_delete_up_to(scope, max_seq): ?array` | `POST /delete_up_to` |
| `scopecache_delete_scope(scope): ?array` | `POST /delete_scope` |
| `scopecache_wipe(): ?array` | `POST /wipe` |
| `scopecache_stats(): ?array` | `GET /stats` |
| `scopecache_scopelist(prefix, after, limit): ?array` | `GET /scopelist` |
| `scopecache_warm(grouped): ?array` | `POST /warm` |
| `scopecache_rebuild(grouped): ?array` | `POST /rebuild` |

Every `?array` function returns the HTTP success envelope as a
PHP-array (canonical shape in [`response_types.go`](../../response_types.go)
/ RFC §6). Payloads are pre-decoded the way `json_decode($body, true)`
would decode them — `{"v":1}` arrives as `['v' => 1]`, not as a raw
JSON string. A `nil` return crosses to PHP as `null` and means "no
caddymodule loaded" (Provision never ran). Operator errors come back
as `['ok' => false, 'error' => '...']`.

## Why

Loopback HTTP to a scopecache running in the same FrankenPHP binary
pays ~3.5 ms of transport for an 11-17 µs cache lookup — ~200×
overhead, all transport. This extension compiles into the same
binary, so PHP→cache calls reach `*Gateway` directly through cgo.
[`bench.sh`](../../tools/frankenphp-ext/bench.sh) measures
`scopecache_get` at ~640 ns / 1.56 M qps for a 54-byte payload;
the in-process route is roughly 1000× cheaper than the same call
over loopback HTTP.

## Files

| file | role |
|---|---|
| [`scopecache_ext.go`](scopecache_ext.go) | The extension source — all `//export_php:function` directives + cgo helpers + hand-rolled JSON-to-zval decoder |
| [`go.mod`](go.mod) | Module pin (with a `replace` directive against the in-repo scopecache source during local builds) |

## Build + validate

```bash
cd tools/frankenphp-ext
./build.sh        # ~1-3 min warm
./smoke.sh        # post-build sanity
./validate.sh     # full correctness suite (~170 checks)
./bench.sh        # per-call latency + throughput
./bench.sh --sweep # scopecache_get cost across payload sizes
```

See [`tools/frankenphp-ext/README.md`](../../tools/frankenphp-ext/README.md)
for the build-chain pitfalls and the runtime details.

## Boundary discipline

- This is an **addon**. The scopecache core (`package scopecache`)
  stays stdlib-only and does not import anything from here.
- The only public surface consumed is `*Gateway` and the typed
  response structs in [`response_types.go`](../../response_types.go).
- The on-the-wire shape of every PHP-array return mirrors the HTTP
  envelope in RFC §6 — single source of truth, no parallel spec.
