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

19 PHP functions, each mirroring an HTTP endpoint on `*Gateway`. Every
function returns a `?string` — the same JSON envelope the matching
HTTP endpoint would emit, byte-identical. Consumers that need an
associative array on the PHP side call `json_decode($result, true)`.
The two `_payload` reads return raw payload bytes (no envelope),
identical to `GET /render`.

| PHP signature | Maps to |
|---|---|
| `scopecache_get(scope, id): ?string` | `GET /get` |
| `scopecache_get_by_seq(scope, seq): ?string` | `GET /get?seq=` |
| `scopecache_get_payload(scope, id): ?string` | `GET /render` (raw bytes, no envelope) |
| `scopecache_get_payload_by_seq(scope, seq): ?string` | `GET /render?seq=` (raw bytes) |
| `scopecache_head(scope, after_seq, limit): ?string` | `GET /head` |
| `scopecache_tail(scope, limit): ?string` | `GET /tail` |
| `scopecache_append(scope, id, payload): ?string` | `POST /append` |
| `scopecache_upsert(scope, id, payload): ?string` | `POST /upsert` |
| `scopecache_update(scope, id, payload): ?string` | `POST /update` |
| `scopecache_counter_add(scope, id, by): ?string` | `POST /counter_add` |
| `scopecache_delete(scope, id): ?string` | `POST /delete` |
| `scopecache_delete_by_seq(scope, seq): ?string` | `POST /delete?seq=` |
| `scopecache_delete_up_to(scope, max_seq): ?string` | `POST /delete_up_to` |
| `scopecache_delete_scope(scope): ?string` | `POST /delete_scope` |
| `scopecache_wipe(): ?string` | `POST /wipe` |
| `scopecache_stats(): ?string` | `GET /stats` |
| `scopecache_scopelist(prefix, after, limit): ?string` | `GET /scopelist` |
| `scopecache_warm(grouped): ?string` | `POST /warm` |
| `scopecache_rebuild(grouped): ?string` | `POST /rebuild` |

A `null` return crosses to PHP as `null` and means "no caddymodule
loaded" (Provision never ran). Operator errors come back as the
envelope `{"ok":false,"error":"..."}`.

The canonical wire shape for every endpoint lives in
[`response_types.go`](../../response_types.go) and RFC §6.

## Choose the right read

Three read paths sit at different price points; pick whichever fits
the consumer:

| function | what you get back | per call | when to use |
|---|---|---:|---|
| `scopecache_get_payload` | raw payload bytes, no envelope | ~208 ns | Forwarding to HTTP, dumping to a frontend, treating cache as a blob store |
| `scopecache_get` | full envelope as JSON-string | ~499 ns | Forwarding the envelope to another service; SSE / WebSocket fan-out |
| `scopecache_get` + `json_decode` | envelope as PHP-array | ~1267 ns | Reading individual fields on the PHP side |

Numbers from `bench.sh` (single thread, 54 B payload, 100k iterations,
warm worker). The PHP `json_decode` step is the dominant cost (~768
ns/call) when you need an array — that cost falls on the consumer
who chose to decode, not on the cache.

## Usage per function

The seed below sets up the scope used by the read examples that
follow. Reads return strings; for clarity each example shows the
JSON output (in `// ...`) and, where useful, the matching `json_decode`
form right beneath.

```php
scopecache_append('users', 'alice', json_encode(['name' => 'Alice', 'age' => 30]));
```

### Reads

#### `scopecache_get(scope, id)`

Read one item by id. Returns the full JSON envelope as a string.

```php
$json = scopecache_get('users', 'alice');
// {"ok":true,"hit":true,"count":1,"item":{"scope":"users","id":"alice","seq":1,"ts":1715600000123456,"payload":{"name":"Alice","age":30}},"approx_response_mb":0.0001}

$env  = json_decode($json, true);
// ['ok' => true, 'hit' => true, 'count' => 1, 'item' => [...], 'approx_response_mb' => 0.0001]
```

Miss returns `{"ok":true,"hit":false,"count":0,"item":null,"approx_response_mb":...}`.

#### `scopecache_get_by_seq(scope, seq)`

Read one item by its assigned seq number. Same envelope shape as
`scopecache_get`.

```php
$json = scopecache_get_by_seq('users', 1);
```

#### `scopecache_get_payload(scope, id)`

Lowest-overhead single-item read: raw payload bytes, no envelope.
`null` on miss (or on no-caddymodule). When the stored payload is a
JSON-string, one layer of JSON-string-encoding is unwrapped so
`<html>...</html>` comes back as the literal bytes.

```php
scopecache_append('pages', 'home', json_encode('<html><body>Hello</body></html>'));

$html = scopecache_get_payload('pages', 'home');
// $html === '<html><body>Hello</body></html>'   // raw string, no envelope
```

#### `scopecache_get_payload_by_seq(scope, seq)`

Same as above, addressed by seq.

```php
$html = scopecache_get_payload_by_seq('pages', 1);
```

#### `scopecache_head(scope, after_seq, limit)`

Read up to `limit` items with `seq > after_seq` in ascending order
(oldest-first within the slice). Pass `after_seq = 0` to start from
the beginning.

```php
$json = scopecache_head('users', 0, 50);
// {"ok":true,"hit":true,"count":1,"truncated":false,"items":[{"scope":"users","id":"alice","seq":1,"ts":...,"payload":{...}}],"approx_response_mb":0.0001}
```

#### `scopecache_tail(scope, limit)`

Read the newest `limit` items in descending seq order.

```php
$json = scopecache_tail('users', 10);
// {"ok":true,"hit":true,"count":1,"offset":0,"truncated":false,"items":[...],"approx_response_mb":0.0001}
```

### Writes

#### `scopecache_append(scope, id, payload)`

Append a new item. `id` must be unique in `scope`, or empty for a
seq-only item (cache assigns a seq, no id). `payload` must be valid
JSON — even single values (`json_encode("foo")`, `json_encode(42)`).
`created` is always `true` on success (append never replaces).

```php
$json = scopecache_append('users', 'bob', json_encode(['name' => 'Bob']));
// {"ok":true,"created":true,"item":{"scope":"users","id":"bob","seq":2,"ts":1715600000234567}}
```

Seq-only append (no id):

```php
$json = scopecache_append('events', '', json_encode(['type' => 'login']));
// {"ok":true,"created":true,"item":{"scope":"events","id":null,"seq":3,"ts":...}}
```

Duplicate-id or invalid-payload → error envelope `{"ok":false,"error":"..."}`.

#### `scopecache_upsert(scope, id, payload)`

Write an item, replacing the payload if `id` already exists.
`created` distinguishes new from replace; on replace the original
`seq` is preserved.

```php
$json = scopecache_upsert('users', 'alice', json_encode(['name' => 'Alice', 'age' => 31]));
// {"ok":true,"created":false,"item":{"scope":"users","id":"alice","seq":1,"ts":...}}

$json = scopecache_upsert('users', 'carol', json_encode(['name' => 'Carol']));
// {"ok":true,"created":true,"item":{"scope":"users","id":"carol","seq":4,"ts":...}}
```

#### `scopecache_update(scope, id, payload)`

Modify payload **only if** the item already exists. `created` is
always `false`. `count` is 1 on hit, 0 on miss (call still succeeds).

```php
$json = scopecache_update('users', 'alice', json_encode(['name' => 'Alice', 'age' => 32]));
// {"ok":true,"created":false,"count":1}
```

#### `scopecache_counter_add(scope, id, by)`

Atomic int64 increment. `by` is a PHP int (negative allowed).
The payload of the targeted item must be (or be becoming) an int.
`created` is `true` on first-touch.

```php
$json = scopecache_counter_add('stats', 'visits', 1);
// {"ok":true,"created":true,"value":1}

$json = scopecache_counter_add('stats', 'visits', 5);
// {"ok":true,"created":false,"value":6}
```

### Deletes

#### `scopecache_delete(scope, id)`

Delete one item by id. `hit` reports whether anything was actually
removed; `count` is 0 or 1.

```php
$json = scopecache_delete('users', 'bob');
// {"ok":true,"hit":true,"count":1}
```

#### `scopecache_delete_by_seq(scope, seq)`

Same as above, addressed by seq.

```php
$json = scopecache_delete_by_seq('users', 4);
// {"ok":true,"hit":true,"count":1}
```

#### `scopecache_delete_up_to(scope, max_seq)`

Bulk-delete every item with `seq <= max_seq` in `scope` — the drain
pattern (subscribe → tail → process → delete_up_to last_processed_seq).

```php
$json = scopecache_delete_up_to('events', 1000);
// {"ok":true,"hit":true,"count":47}
```

#### `scopecache_delete_scope(scope)`

Drop the entire scope. `hit` is "did the scope exist before". An
empty-but-existing scope still counts as a hit.

```php
$json = scopecache_delete_scope('users');
// {"ok":true,"hit":true,"count":3}
```

#### `scopecache_wipe()`

Drop **every** non-reserved scope. Reserved scopes (`_events`,
`_inbox`) are immediately re-created under the same all-shard lock,
so subscribers don't observe a gap. Returns the totals freed.

```php
$json = scopecache_wipe();
// {"ok":true,"scopes":8,"items":152,"freed_mb":0.0421}
```

### Bulk

#### `scopecache_warm(grouped)`

Atomically replace the contents of one or more scopes. `grouped` is
a PHP-assoc array keyed by scope name; each value is a list of
`['id' => ..., 'payload' => ...]` entries. `id` may be omitted for
seq-only items. Whole call rolls back on any per-entry error.

```php
$json = scopecache_warm([
    'pages' => [
        ['id' => 'home',  'payload' => json_encode('<html>...</html>')],
        ['id' => 'about', 'payload' => json_encode('<html>...</html>')],
    ],
    'flags' => [
        ['id' => 'feature_x', 'payload' => json_encode(true)],
    ],
]);
// {"ok":true,"scopes":2}
```

#### `scopecache_rebuild(grouped)`

Same input shape as `warm`, but applies as a full-cache replacement:
any scope not present in `grouped` is wiped first. Reserved scopes
are preserved across the cycle.

```php
$json = scopecache_rebuild([
    'pages' => [ /* ... */ ],
]);
// {"ok":true,"scopes":1,"items":4}
```

### Observability

#### `scopecache_stats()`

Snapshot of the whole cache. No args.

```php
$json = scopecache_stats();
// {"ok":true,"scopes":4,"items":12,"approx_store_mb":0.0034,"last_write_ts":1715600000999000,"events_drops_total":0,"reserved_scopes":[{"scope":"_events","item_count":12,...},{"scope":"_inbox","item_count":0,...}]}
```

#### `scopecache_scopelist(prefix, after, limit)`

Paginated list of scopes. `prefix` filters by leading substring
(empty = all). `after` is the last scope name from the previous page
(empty = start). Each entry carries the scope's item-count and
byte-footprint.

```php
$json = scopecache_scopelist('', '', 100);
// {"ok":true,"hit":true,"count":4,"truncated":false,"scopes":[{"scope":"_events","item_count":12,"approx_scope_mb":0.0021,...},...],"approx_response_mb":0.0002}
```

## Why

Loopback HTTP to a scopecache running in the same FrankenPHP binary
pays ~3.5 ms of transport for an 11-17 µs cache lookup — ~200×
overhead, all transport. This extension compiles into the same
binary, so PHP→cache calls reach `*Gateway` directly through cgo.
[`bench.sh`](../../tools/frankenphp-ext/bench.sh) measures
`scopecache_get_payload` at ~208 ns / 4.8 M qps and the full-envelope
`scopecache_get` at ~499 ns / 2.0 M qps for a 54-byte payload; the
in-process route is roughly 1000× cheaper than the same call over
loopback HTTP.

## Files

| file | role |
|---|---|
| [`scopecache_ext.go`](scopecache_ext.go) | The extension source — all `//export_php:function` directives + cgo helpers. Returns are `?string` (JSON envelopes from `wire.go` for reads, `MarshalEnvelope` for everything else). |
| [`go.mod`](go.mod) | Module pin (with a `replace` directive against the in-repo scopecache source during local builds) |

## Build + validate

```bash
cd tools/frankenphp-ext
./build.sh        # ~1-3 min warm
./smoke.sh        # post-build sanity
./validate.sh     # full correctness suite
./bench.sh        # per-call latency + throughput
./bench.sh --sweep # scopecache_get cost across payload sizes
```

See [`tools/frankenphp-ext/README.md`](../../tools/frankenphp-ext/README.md)
for the build-chain pitfalls and the runtime details.

## Boundary discipline

- This is an **addon**. The scopecache core (`package scopecache`)
  stays stdlib-only and does not import anything from here.
- The only public surface consumed is `*Gateway`, the typed response
  structs in [`response_types.go`](../../response_types.go), and the
  public byte-builders in [`wire.go`](../../wire.go).
- Every return is the same wire shape the HTTP endpoint would emit —
  single source of truth in RFC §6.
