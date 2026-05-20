# ScopeCache

ScopeCache is a lightweight in-memory datastore/cache and write buffer for Caddy: a simple, scope-partitioned, ordered datastore built for ultra-fast dynamic reads. ScopeCache combines key-value style access with ordered per-scope collections. It runs primarily as a Caddy module, but can also run standalone over a Unix socket.

It stores application data in memory and lets Caddy serve selected reads directly from the web server process. This can reduce pressure on both the database and the application layer.

ScopeCache is not a Redis replacement and not an HTTP response cache. It is an application-managed datastore organized by scope: your application decides what data is stored, under which scope, and when it is updated or removed.

## How ScopeCache works

Traditional hot-read paths often look like this:

```text
Caddy -> application runtime (Node.js/PHP/etc.) -> Redis/database -> application runtime -> response
```

ScopeCache allows selected reads to take a shorter path:

```text
Caddy -> ScopeCache memory -> response
```

When ScopeCache is compiled into Caddy, it runs in the same OS process as the web server. There is no Redis protocol, no cache-service roundtrip, and no separate cache process on the hot read path.

In benchmark tests, this direct in-process path served data about 9× faster than routing the same request through Node.js and Redis on the same server. The relevant difference is architectural: ScopeCache removes parts of the request path. It does not claim that Redis itself is slow.

A typical request:

```text
GET /tail?scope=thread:123&limit=100
```

returns the latest 100 items for the scope `thread:123`.

**Direct PHP access through FrankenPHP**

FrankenPHP makes it possible to write PHP extensions in Go, so PHP can access ScopeCache directly inside the same process. 

In a end-to-end benchmark, a PHP `scopecache_get()` call to ScopeCache and back took around **0.5 microseconds**. A regular PHP-to-Redis roundtrip on the same server took around **127 microseconds** with a persistent Redis connection, while opening a new Redis connection for a single request took roughly **600 microseconds**.

The difference is architectural: ScopeCache avoids the extra process and protocol roundtrip that Redis requires.

ScopeCache is not tied to PHP. The same in-process Caddy/cache architecture can be useful in many other use cases and with other platforms as well.

## What ScopeCache is

ScopeCache is a small in-memory datastore/cache for workloads where the application already knows which views need to be served quickly.

Good examples include:

- latest messages or reactions in a thread
- private inbox views
- unread counters
- notification lists
- view counters
- rate-limit buckets
- pre-rendered HTML fragments for HTMX-driven interfaces
- small materialized JSON, HTML, XML, or text views
- high-frequency events that need to be drained later

The cache is intentionally disposable. Your source of truth lives elsewhere — usually a database, but it can be any external store (a JSON file, in-memory state, an external API). ScopeCache can be wiped, warmed, or rebuilt from it at any time.

## What ScopeCache is not

ScopeCache is not a general-purpose Redis replacement. Redis and similar systems offer a much broader feature set: richer data types, more commands, clustering, persistence options, replication, mature operational tooling, and a large ecosystem.

ScopeCache is also not a traditional HTTP response cache like Varnish or Souin. It does not cache responses automatically based on incoming URLs or cache-control headers.

Instead, your application publishes prepared data into named scopes, and Caddy can serve that data directly.

```text
application -> ScopeCache scope/id -> Caddy serves it
```

## Core model

ScopeCache stores items inside scopes.

A scope is a named partition, similar to a namespace or bucket. Each scope contains an ordered collection of items. Items are addressable only through official top-level fields:

- `scope` — required partition key
- `id` — optional stable application-owned identifier
- `seq` — cache-owned sequence number, monotonically increasing per scope, assigned by ScopeCache on every append
- `ts` — cache-owned microsecond timestamp, set by ScopeCache on every write; observability only, not searchable and not used for ordering
- `payload` — required JSON value, treated as opaque application data

ScopeCache does not inspect the payload for filtering or querying. Filtering, addressing, and cursoring only operate on official top-level item fields. IDs, when present, are plain strings whose meaning is decided by the application.

## Limited filtering, flexible access

ScopeCache deliberately avoids becoming a query engine.

There is no query language, no joins, no arbitrary predicates, no sorting DSL, and no payload inspection beyond validation at write time.

Instead, flexibility comes from creating materialized views ahead of time:

```text
thread:34
user:42:inbox
user:42:unread
tenant:acme:thread:34
```

If you need “all unread notifications for user 42”, your application stores that view under a scope such as:

```text
user:42:unread
```

ScopeCache then serves that scope quickly. It does not search through payloads to discover it.

Although filtering is limited, ScopeCache remains flexible — and usable for most real-world use cases — through scope and ID naming. Instead of searching inside payloads, the application encodes access patterns directly into scope names.

Because each scope is ordered by its cache-assigned `seq`, retrieving the latest 100 unread messages for that user is a native operation:

```text
/tail?scope=user:42:unread&limit=100
```

**ScopeCache combines key-value style access with ordered per-scope collections.** Direct lookups by `id` or `seq` behave like simple key-value reads, while the built-in sequence order makes operations such as `tail`, `head`, and `since(seq)` natural core primitives.

## Main use cases

### 1. Read cache

ScopeCache can serve hot dynamic data directly from Caddy.

This is useful when normal HTTP response caching is inconvenient or too coarse-grained, for example:

- private user-specific data
- frequently changing thread data
- inboxes
- notifications
- counters
- pre-rendered fragments

Your application prepares the data and writes it into ScopeCache. Caddy then serves selected reads without involving the application runtime.

### 2. Write buffer

ScopeCache can also buffer high-frequency events such as analytics hits, log lines, or chat messages.

A worker can drain the buffer in batches using endpoints such as `/tail` and `/delete_up_to`, then process, persist, or forward the data elsewhere.

ScopeCache also includes a subscription model for change notifications, so an external process can be notified when new data is available.

### 3. Fronting proxy for prepared content

ScopeCache can serve cached HTML, JSON, XML, or text directly through `/render`.

This may look similar to an HTTP response cache, but the model is different. ScopeCache does not decide what to cache from incoming requests. Your application precomputes the content, stores it under a scope and ID, and Caddy serves it directly when requested.

## Core endpoints

The core HTTP API is intentionally small.

- **Read:** `get`, `render`, `head`, `tail`
- **Write:** `append`, `upsert`, `update`
- **Bulk / load:** `warm`, `rebuild`
- **Cleanup:** `delete`, `delete_up_to`, `delete_scope`, `wipe`
- **Observe:** `stats`, `scopelist`, `help`

The exact endpoint contracts are documented in [`docs/scopecache-core-rfc.md`](docs/scopecache-core-rfc.md).

## Why ScopeCache exists

### A lightweight in-memory datastore for hot views

Redis is excellent software. But for narrow hot-read patterns, it can be more infrastructure than the workload requires.

Redis is often used to reduce pressure on the database, but the request still usually passes through the application layer:

```text
web server -> application -> Redis -> application -> response
```

ScopeCache is built for cases where the cache can safely be disposable, rebuilt from the source of truth, and served directly from the web server process.

For suitable read paths, ScopeCache can reduce pressure on both:

- the database
- the application runtime

That means fewer moving parts, lower per-request overhead, and higher HTTP throughput for the specific paths that fit this model.

### FrankenPHP

The other major motivation for ScopeCache is [FrankenPHP](https://frankenphp.dev/).

FrankenPHP shows how powerful a Caddy-based architecture can be when more of the web stack runs together. Its worker mode improves PHP performance by keeping application workers alive in memory, avoiding much of the overhead of traditional per-request PHP execution.

FrankenPHP also makes distribution simpler: a PHP application can be packaged into a single binary that includes Caddy and Mercure for Server-Sent Events. No separate installation and configuration of a web server or PHP runtime is required.

But when Redis is required for optimal performance, that distribution model becomes less self-contained. Redis remains a separate service that must be run, secured, monitored, and maintained. It also cannot be compiled into the same single binary as the PHP application, web server, and Mercure.

FrankenPHP reduces one service boundary by bringing the PHP runtime closer to Caddy. ScopeCache applies a related idea to data: keep frequently accessed data inside the web server process and avoid an additional cache-service roundtrip on the read path.

Because ScopeCache is a Caddy module, it can be compiled into the same custom FrankenPHP/Caddy binary as Caddy, the PHP runtime, and the SSE hub.

### PHP extensions written in Go

One important advantage of FrankenPHP’s design is that PHP extensions can be written in Go. A small Go file with `//export_php:function` directives can be processed by a generator, making Go functions available in PHP as native function calls.

Because FrankenPHP and Caddy are tightly integrated, Caddy, PHP, and ScopeCache run inside the same OS process: one PID, one address space.

That matters. A PHP extension written in Go can call ScopeCache without leaving the process. Compared with a typical PHP + Redis setup, this removes the Redis protocol, socket roundtrip, and separate Redis process from the access path.

There is still a PHP-to-Go extension boundary, including type conversion. But the path is much shorter than a typical PHP-to-Redis lookup.

In an initial benchmark, a `scopecache_get()` call exposed through a Go extension took about 0.7 µs on average. A single PHP-to-Redis lookup that opened and closed its connection took roughly 581 µs. With a persistent Redis connection, that dropped to about 126 µs in steady state.

Even in that best-case Redis scenario, the measured PHP-to-Redis access path was still about 452× slower than the equivalent in-process ScopeCache call.

These numbers measure the complete access path for this specific workload. They do not mean that ScopeCache’s internal lookup algorithm is hundreds of times faster than Redis internally.

To fully unlock the potential of FrankenPHP, you need more than an embedded PHP runtime. You also need application data that can live inside the same process as the application code. ScopeCache is built for that role.

## Internals

### Storage model

ScopeCache’s internal storage model is deliberately simple.

Each scope owns one ordered slice of items, stored in append order. Around that slice, ScopeCache maintains lightweight hashmap indexes for direct lookup by `id` and `seq`.

Conceptually, the core shape is:

```go
type scopeBuffer struct {
    items []*Item             // primary storage, in append order
    byID  map[string]*Item    // id  -> item
    bySeq map[uint64]*Item    // seq -> item
    mu    sync.RWMutex        // one lock per scope
}
```

The slice is the ordered storage. It defines the physical order of the data in memory and makes operations such as `head`, `tail`, and cursor-based reads natural.

The maps exist to avoid scanning. A lookup by `id` or `seq` is an O(1) hashmap lookup on average, independent of the number of items in the scope.

The slice and both maps hold pointers to the same items, so each item lives in memory once, no matter how many indexes address it.

A classical key-value store is conceptually built around:

```text
key -> value
```

Ordering is not part of that basic model. If you want “the latest 10 items”, you usually build that on top with lists, streams, sorted sets, timestamps, or secondary indexes.

ScopeCache starts from a different shape:

```text
scope -> ordered collection -> indexed items
```

Each scope is an ordered collection first, with direct lookup indexes around it. That is why operations such as `head`, `tail`, and cursor-based reads are native core primitives rather than conventions layered on top of a flat keyspace.

### Locking and sharding

Internally, the top-level store is sharded by scope name. A request may briefly touch a shard-level lock to find the scope buffer. After that, the operation is handled by the scope’s own buffer.

That means unrelated scopes do not block each other during normal per-scope operations.

```text
scope "thread:1" -> own buffer -> own lock
scope "thread:2" -> own buffer -> own lock
scope "user:42"  -> own buffer -> own lock
```

Reads share a per-scope read lock, so multiple reads on the same scope can run concurrently. Writes take the per-scope write lock for the duration of the mutation.

This matches the data model: scopes are not only names, but natural concurrency partitions.

### Performance shape

A request such as:

```text
GET /get?scope=X&seq=N
```

resolves conceptually as:

```text
scope lookup -> bySeq lookup -> item
```

Both lookup steps are O(1) on average. Ordered reads such as `/head` and `/tail` walk the `items` slice instead.

A single in-process lookup can take tens of nanoseconds. In one benchmark, `getBySeq` took about 43 ns per lookup on a single CPU core, roughly 23 million lookups per second. Because each scope has its own read/write lock, reads on different scopes scale independently across cores; reads on the same scope share a read-lock and also run concurrently.

These internal numbers refer only to the in-process lookup itself. They do not include Caddy routing, HTTP request parsing, response writing, JSON encoding, or network overhead.

The larger HTTP benchmark numbers in this README measure complete request paths.

## Addons and extension model

Higher-level behavior stays outside the core.

Examples include:

- multi-tenant gateways
- batch dispatchers
- write-only ingestion
- custom authentication
- access control
- persistence bridges
- event processors
- custom lifecycle/draining behavior

ScopeCache exposes a public Go API surface through `*Gateway`. Addons build on top of that boundary instead of reaching directly into internal types such as `*store` or `*scopeBuffer`.

The gateway exists for three concrete reasons.

### 1. Stable boundary

The core can rename, reshape, or replace internal types between versions without breaking addons that depend only on `*Gateway`.

Internals are free to evolve. The addon contract stays stable.

### 2. Defensive cloning

Byte slices that cross the gateway boundary are copied in both directions. This prevents addons from mutating internal cache buffers and prevents ScopeCache from mutating an addon’s input buffers.

Internal call paths inside the core can skip this copy. Cloning is a boundary concern, not a per-call tax inside the core.

### 3. Uniform validation

Shape rules such as scope length, payload size, and JSON validity live below the gateway, so every entry into the cache passes through the same checks.

An addon cannot bypass validation that an HTTP caller faces.

## Built-in convenience mechanisms

ScopeCache includes two convenience mechanisms around the core:

- a warm-up script hook that can run when the web server starts
- a subscription model that can notify external processes when data changes

The subscription model is useful for draining and persistence workflows. A worker can be notified when new items are appended, drain them in batches, process them, and persist them elsewhere.

Depending on the use case, draining can happen immediately or after a short delay so items can be processed in batches.

## Quickstart: Docker

Run Caddy with ScopeCache baked in on `localhost:8081`:

```bash
git clone https://github.com/VeloxCoding/scopecache.git
cd scopecache
docker compose up -d --build caddyscope
curl http://localhost:8081/help
```

The bundled [`deploy/Caddyfile.caddyscope`](deploy/Caddyfile.caddyscope) is already wired for GET and POST:

```caddyfile
:8080 {
    scopecache {
        scope_max_items 100000
        max_store_mb    100
        max_item_mb     1
    }
    respond 404
}
```

Explanation:

- `:8080 { ... }` — Caddy listens on port 8080 inside the container; Docker Compose maps that to 8081 on your host.
- `scopecache { ... }` — ScopeCache endpoints are mounted at `/`, so `GET /help`, `GET /tail`, `POST /append`, and other endpoints are available.
- `respond 404` — anything ScopeCache does not recognize returns 404.
- `scope_max_items`, `max_store_mb`, and `max_item_mb` are capacity limits.

### Example: POST and GET round-trip

```bash
# Write an item.
curl -X POST http://localhost:8081/append \
  -H 'Content-Type: application/json' \
  -d '{"scope":"demo","payload":{"msg":"hello"}}'

# Read it back.
curl 'http://localhost:8081/tail?scope=demo'
```

The `xcaddy` build recipe for a custom Caddy binary lives in [`Dockerfile.caddyscope`](Dockerfile.caddyscope).

## Quickstart: VPS install

A one-shot installer brings up Caddy with the ScopeCache module on a fresh Ubuntu/Debian VPS:

```bash
curl -fsSL https://raw.githubusercontent.com/VeloxCoding/scopecache/main/scripts/install_caddyscope.sh | sudo bash
```

When the installer finishes:

- Caddy + ScopeCache is running on `:80`.
- The systemd unit `caddy.service` starts automatically on reboot and restarts on crash.
- `wrk` is installed for the benchmark step below.

Run a 5-second load test against the cache using random GET requests on a 50K-item dataset:

```bash
curl -fsSL https://raw.githubusercontent.com/VeloxCoding/scopecache/main/scripts/run_benchmark.sh | bash
```

For configuration knobs such as port, version, capacity caps, and benchmark tuning, see [`docs/install_caddyscope.md`](docs/install_caddyscope.md).

## Performance

Benchmark results are hardware-dependent. The numbers below were measured on an AMD Ryzen AI Max+ 395 with 32 GB of LPDDR5X-8000.

The point of this comparison is the relative gap between request paths, not the absolute throughput.

A side-by-side benchmark compared three HTTP read paths under identical `wrk -t4 -c64 -d5s` load on the same host, using a 50,000-item dataset and 10-run averages:

| Route | Requests/sec | p50 latency |
|---|---:|---:|
| Caddy -> Node.js -> Redis (Unix socket) | 30,414 | 1.870 ms |
| Caddy/FrankenPHP worker -> Redis | 30,543 | 1.969 ms |
| Caddy -> ScopeCache (in-process) | **281,511** | **0.138 ms** |

ScopeCache reached about 9× the throughput of either Redis-backed route in this benchmark.

Again, this is an architectural comparison, not a claim that Redis itself is slow. Redis is extremely fast internally. The difference here is that ScopeCache removes the application-runtime hop and Redis roundtrip from the selected read path.

## Status

ScopeCache is pre-1.0.

The core HTTP and Go API surfaces may still change between minor versions. After v1.0, the core API is intended to become semver-stable.

## Building from source

```bash
go build -o scopecache ./cmd/scopecache
go test ./...
```

Module path:

```text
github.com/VeloxCoding/scopecache
```

ScopeCache currently uses only the Go standard library.

## Documentation

The full design, endpoint contracts, and architectural rationale live in [`docs/scopecache-core-rfc.md`](docs/scopecache-core-rfc.md).

## License

Apache License, Version 2.0. See [`LICENSE`](LICENSE).

Copyright 2026 VeloxCoding.
