# scopecache

ScopeCache is an in-memory cache that runs inside Caddy. It lets Caddy serve selected dynamic reads directly from memory. This can reduce pressure on both the database and application layer.

ScopeCache works standalone over plain HTTP, for example over a Unix socket, from any programming language. But it is designed to shine as a Caddy module: when installed inside Caddy, ScopeCache runs in the same process as the webserver, allowing Caddy to serve ScopeCache data directly from memory.

This avoids the usual request path through separate application and storage processes on requests, such as PHP, Node.js, Redis, or a database. In benchmark tests, this direct in-process path served data about 7× faster than routing the same request through Node.js and Redis, even though all services were installed on the same server.

Redis - and similar in-memory data stores - are fast by themselve. But a typical cache 'read' is not just a Redis memory lookup. The request first moves from the webserver into an application process, where a Redis client sends a command to the Redis process over TCP or a Unix socket. Redis returns the data to the application, which then builds the HTTP response and passes it back to the webserver. ScopeCache avoids this roundtrip by running inside Caddy itself.

By removing separate application and storage services from the request path, ScopeCache can reach a latency and throughput profile that traditional multi-service request paths cannot realistically match.

ScopeCache deliberately keeps filtering limited. The core only addresses a few official top-level fields, which keeps it robust, predictable, and fast. But ScopeCache remains flexible enough for many real-world use cases.

For example, the following request returns the latest 100 reactions for thread `123`:

```
https://example.com/tail?scope=thread:123&limit=100
```

ScopeCache is not a replacement for Redis or similar in-memory data stores. Those systems offer a much broader feature set: richer data types, more commands, clustering, and mature operational tooling. Their raw performance is not the limiting factor in this comparison. The relevant difference is the request path around them. Even on the same server, crossing application- and service boundaries adds latency and reduces the number of HTTP requests the system can serve.

ScopeCache is not an HTTP response cache like Varnish or Souin. It is a scope-addressed publish cache: your application decides what data is stored, under which scope, and when it is updated or removed.

ScopeCache can also act as a write buffer, with built-in support for change notifications so external scripts can drain, process, or persist events elsewhere.

## Why ScopeCache Exists

ScopeCache targets workloads where an application needs a hot, scope-addressed views in memory rather than a full general-purpose datastore. Examples include latest thread messages, unread counters, inbox views, view counts, rate-limit buckets, pre-rendered HTML fragments for HTMX-driven interfaces, and small materialized views.

Redis is excellent software, but for these narrow patterns it can be more infrastructure than the workload requires. ScopeCache is built for cases where the cache can safely be disposable, rebuilt from the source of truth, and served directly from the webserver process. Because ScopeCache runs inside Caddy, cache reads can be served directly from the webserver process. For suitable hot-read paths, this can remove the need for a separate cache service and reduce pressure on the application runtime, resulting in fewer moving parts, lower resource usage, and higher HTTP throughput. Redis is typically used in order to reduce pressure on the database, but requests still pass through the application layer. ScopeCache can reduce pressure on both the database and the application layer, because suitable cache reads can be served directly by Caddy.

The second reason ScopeCache exists is [FrankenPHP](https://frankenphp.dev/).

FrankenPHP shows how powerful a Caddy-based architecture can be when more of the web stack runs together. Its worker mode improves PHP performance by keeping application workers alive in memory, avoiding much of the overhead of traditional per-request PHP execution.

FrankenPHP also makes distribution simpler: a PHP application can be packaged into a single binary that includes Caddy and Mercure for Server-Sent Events. No separate installation and configuration of a web server or PHP runtime is required.

But when Redis is required for optimal performance, that distribution model breaks down. Redis remains a separate service that must be run, secured, monitored, and maintained. It also cannot be compiled into the same single binary as the PHP application, web server, and Mercure. 

FrankenPHP reduces one service boundary by bringing the PHP runtime closer to Caddy. ScopeCache applies a related idea to data: keep frequently accessed data inside the web server process and avoid an additional cache-service roundtrip on the read path. Also important: because ScopeCache is a Caddy module, it can be compiled into the same custom FrankenPHP/Caddy binary as Caddy, the PHP runtime, and the SSE hub.

## What it is: Core features and Addons

As stated above, ScopeCache is a in-memory publish cache and write buffer. It can hold a slice of your data in RAM in front of your persistent data store. Items live inside *scopes* — what other systems call a namespace or bucket — and are addressable only by `scope`, `id`, or `seq`. The entire cache is wipeable and rebuildable from the source of
truth at any time. 

Because scope names and IDs are plain strings, ScopeCache remains flexible without adding a query language. The application can encode its own domain model into names such as:

```text
thread:34                    -> latest messages or reactions in a thread
user:42:inbox                -> private inbox data for one user
tenant:acme:thread:34        -> thread data scoped to one tenant
article:hello-world:comments -> comments for one article
chat:user:42:user:77         -> messages in a private chat
counter:thread:34            -> counters for one thread
```

Item IDs are optional. ScopeCache automatically assigns a `seq` value to every item, so items can always be ordered and addressed by sequence.

Optional IDs let the application give items a stable, meaningful name, such as `reaction:884`, `comment:102`, `notification:183`, `views`, or `likes`.

ScopeCache treats both scope names and IDs as plain strings. It does not inspect or filter on parts of the ID, but meaningful IDs make it easy for the application to address, update, delete, or organize items.

ScopeCache deliberately keeps filtering limited. It will not grow into a query engine. Flexibility comes from creating materialized views of your data: the application prepares the views that clients need and stores them under separate, purpose-specific scopes such as `thread:34`, `user:42:inbox`, or `user:42:unread`.

ScopeCache has three main use cases:

- **Read cache.** A traditional HTTP response cache like Varnish works well for public responses that can be cached by URL and cache-control rules. But for frequently changing data or private data a response cache is often less convenient, and sometimes the wrong tool entirely.
  ScopeCache can store data under scopes and IDs. That makes it useful for things like new chat messages for a specific user, the latest reactions in a thread, private inbox data, view-counters and notifications.
- **Write buffer.** ScopeCache can buffer high-frequency events such as analytics hits, log lines, or chat messages. A background worker can drain the buffer in batches using `/tail` and `/delete_up_to`, then process, persist, or forward the data elsewhere. ScopeCache also includes a subscription model for change notifications, so an external script or worker can be notified when new data is available for processing.
- **Fronting proxy.** A webserver with scopecache can serve cached HTML, JSON, or XML directly from `/render`, without involving the application layer. This may look similar to an HTTP response cache, but the model is different: ScopeCache does not cache responses based on incoming requests. Instead, your application precomputes the content, stores it under a scope and ID, and the proxy serves that content directly when requested. 

ScopeCache provides two built-in convenience mechanisms around the core:

- A build-up script can run automatically when the webserver starts, making it easy to rebuild or warm the cache after a restart.
- A subscription model can notify external applications or scripts when data changes, so they can drain, process, persist, or otherwise handle incoming data.

Apart from the two convenience features mentioned above, the core is intentionally limited to a small set of HTTP endpoints:

- **Read:** `get`, `head`, `tail`
- **Write / load:** `append`, `warm`, `rebuild`
- **Cleanup:** `delete`, `delete_up_to`, `delete_scope`
- **Observe:** `stats`, `scopelist`

### ScopeCache 'internals'

ScopeCache deliberately limits filtering to three axes: `scope`, `id`, and `seq`. There is no query language, no joins, and no payload inspection (apart from JSON + UTF-8 validity checks at write time). There is no TTL system and when the configured memory limit is reached, ScopeCache fails writes explicitly with HTTP `507 Insufficient Storage` instead of silently deleting data in the background. 

Internally, the top-level store is a 32-shard map keyed by scope name. A write may briefly touch a shard-level lock to find the
scope, but after that the operation is handled by the scope's own buffer. That means unrelated scopes do not block each other during the actual data mutation.

Each scope stores items in an append-oriented slice, with two parallel indexes: one keyed by seq, and one keyed by id.

A request such as `GET /get?scope=X&seq=N` resolves in two hash-map lookups: a top-level shard-map lookup to find the scope buffer, then a `bySeq` map lookup that returns the item directly. Both steps are O(1) on average, independent of scope size — about 43 ns per lookup, roughly 23 million per second on a single CPU core. (Ordered traversals such as `/head` and `/tail` walk the `items` slice instead; the maps are unused for those endpoints.)

Reads share a per-scope read-lock so multiple lookups run concurrently. On multicore hardware, aggregate throughput rises to roughly 77 million lookups per second at 8 cores — about 13 ns per lookup — and plateaus there: read-lock contention prevents further linear scaling beyond that point.

That performance is not accidental. It comes from both performance tuning and a deliberately small core: no query language, no joins, no payload inspection, and no application logic in the hot path. ScopeCache stays fast and predictable by rejecting features that would add flexibility at the cost of speed, simplicity, or stability.

This limitation of the core functionality of ScopeCache is intentional: ScopeCache keeps the core small, stable, and predictable, while leaving higher-level behavior to modules and addons. The core and the two features are heavily tested, validated, optimized and benchmarked. 

P.S. These number refer only to the internal in-process lookup, not to a full request through Caddy's routing, request parsing, response writing.  

### Addons

Higher-level behavior — such as multi-tenant gateways, batch dispatchers, write-only ingestion, custom authentication, access control, persistence, or event processing — stays outside the core and can be implemented as separate addon packages.

ScopeCache exposes a single public Go API surface — `*Gateway` — that addons build on top of. Internal types (`*store`, `*scopeBuffer`, and other lowercase identifiers in the core package) are not reachable from outside the package, so addons physically cannot reach into them.

The gateway layer exists for three concrete reasons:

1. **Stable boundary.** The core can rename, reshape, or replace its internal types between versions without breaking any addon that depends only on `*Gateway`. Internals are free to evolve; the contract with addons does not. More importantly, new addons can be built without worrying about ScopeCache's internals — memory sharding, lock discipline, byte-budget accounting, or any other implementation detail below `*Gateway`.
2. **Defensive cloning.** Every byte slice that crosses the gateway is copied in both directions, so internal buffers can never be mutated by an addon, and an addon's byte slices can never be mutated by the cache. Internal call paths inside the core skip the copy — cloning is a boundary concern, not a per-call tax.
3. **Uniform validation.** Shape rules (scope length, payload size, JSON validity) live at the `*store` boundary below `*Gateway`, so every entry into the cache — HTTP request or Go API call — passes through the same checks. An addon cannot bypass validation that an HTTP caller faces.

The result: the core stays stable, fast, and predictable, while ScopeCache remains straightforward to extend for application-specific needs.

For example, an authorization addon can validate a bearer token, map it to the scopes that token may access, and then return only the items from those allowed scopes — all without touching anything below `*Gateway`.

There is no TTL system built into ScopeCache, but it is easy to implement lifecycle or draining behavior outside the core. ScopeCache includes a built-in subscription model, allowing an external process to be notified when new items are appended. That process can drain the new items, process them, and persist them elsewhere. Depending on the use case, draining can happen immediately or after a short delay — for example 0.5 seconds or longer — so items can be processed in batches.

So, ScopeCache is built around a modular architecture. Addons interact with the cache through a clear built-in gateway API, instead of reaching directly into the internal core.
For example, an authorization/access addon had been built that validates a bearer token and then returns only the items from the scopes that token is allowed to access.


## Quickstart Docker install

Caddy with scopecache baked in, served on `localhost:8081`:

```bash
git clone https://github.com/VeloxCoding/scopecache.git
cd scopecache
docker compose up -d --build caddyscope
curl http://localhost:8081/help
```

The bundled [deploy/Caddyfile.caddyscope](deploy/Caddyfile.caddyscope)
is already wired for GET and POST:

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

- `:8080 { ... }` — Caddy listens on port 8080 inside the container;
  docker-compose maps that to 8081 on your host.
- `scopecache { ... }` — every scopecache endpoint is mounted at `/`
  (so `GET /help`, `GET /tail`, `POST /append`, …).
- `respond 404` — anything scopecache doesn't recognise → 404.
- The three knobs inside are capacity limits only; they don't
  restrict GET/POST. Every verb scopecache supports just works.

**Example: POST and GET round-trip**

```bash
# write an item
curl -X POST http://localhost:8081/append \
  -H 'Content-Type: application/json' \
  -d '{"scope":"demo","payload":{"msg":"hello"}}'

# read it back
curl 'http://localhost:8081/tail?scope=demo'
```

The xcaddy build recipe for your own Caddy binary lives in
[Dockerfile.caddyscope](Dockerfile.caddyscope).


## Quickstart VPS install

A one-shot installer brings up Caddy with the scopecache module on a
fresh Ubuntu/Debian VPS:

```bash
curl -fsSL https://raw.githubusercontent.com/VeloxCoding/scopecache/main/scripts/install_caddyscope.sh | sudo bash
```

When that finishes:

- Caddy + scopecache is running on `:80`.
- The systemd unit `caddy.service` auto-starts on reboot and restarts
  on crash.
- `wrk` is installed (for the benchmark step below).

A separate one-liner runs a 5 sec load test against the cache (random GET requests on a 50K item dataset):

```bash
curl -fsSL https://raw.githubusercontent.com/VeloxCoding/scopecache/main/scripts/run_benchmark.sh | bash
```

For configuration knobs (port, version, capacity caps, benchmark
tuning), see [docs/install_caddyscope.md](docs/install_caddyscope.md).


## Performance

A side-by-side benchmark comparing three HTTP read paths under
identical `wrk -t4 -c64 -d5s` load on the same host (50,000-item
dataset, 10 runs averaged):

| Route | Requests/sec | p50 latency |
|---|---:|---:|
| Caddy → Node.js → Redis (Unix socket) | 30,414 | 1.870 ms |
| Caddy/FrankenPHP worker → Redis | 30,543 | 1.969 ms |
| Caddy → scopecache (in-process) | **222,554** | **0.187 ms** |

Scopecache reaches ~7.3× the throughput of either Redis-backed route.
The win is **architectural, not a Redis-vs-scopecache speed comparison**:
running the cache inside the Caddy process removes the application-runtime hop and the Redis roundtrip from the read path entirely. A
single in-process `getBySeq` lookup itself takes ~43 ns regardless of
scope size (hash-map, O(1)) — about 23 million lookups per second per
core.

These figures are hardware-dependent. The numbers above were
measured on an AMD Ryzen AI Max+ 395 with 32 GB of LPDDR5X-8000;
the same random-seq /get workload on a 4-vCPU / 8 GB VPS reaches
roughly 90,000 req/s. The point of this comparison is the
**relative gap between the three routes, not the absolute
throughput** — and that gap holds across hardware tiers.

### Resource utilization

Higher throughput would not matter if it cost proportionally more CPU and RAM — a leaner route can be scaled horizontally to match. So a separate run captured per-process CPU usage (via `/proc` jiffies) and memory (via `/proc/[pid]/status` RSS) alongside throughput; the per-route ratios sit in one combined table:

| Route | Requests/sec | Server CPU (cores avg) | Req/sec per core | Memory (MiB avg) | Req/sec per MiB |
|---|---:|---:|---:|---:|---:|
| Caddy → Node → Redis | 31,452 | 10.52 | ~2,989 | ~292 | ~108 |
| Caddy → FrankenPHP worker → Redis | 31,451 | 6.67 | ~4,716 | ~71 | ~443 |
| Caddy → ScopeCache | **227,979** | 10.31 | **~22,121** | ~83 | **~2,747** |

Two findings sit underneath the headline 7.25× throughput gap:

- **Per CPU core**, ScopeCache is ~7.4× more efficient than Node and ~4.7× more efficient than FrankenPHP worker mode.
- **Per MiB of memory**, ScopeCache is ~25× more efficient than Node → Redis and ~6.2× more than FrankenPHP worker → Redis.

Redis itself was never the bottleneck (0.42 core in the Node route, 0.91 in the FrankenPHP route). Most of the CPU went into the path *around* Redis — Caddy, the application runtime, protocol handling, decoding, response construction. Removing that path is what makes ScopeCache competitive: Caddy answers directly from its own process memory, with no application-runtime hop and no external cache roundtrip.

Full methodology, hardware, container/CPU pinning, and per-percentile
results in [docs/benchmark_roundtrip.md](docs/benchmark_roundtrip.md).



## Status

Pre-1.0. The core HTTP and Go API surfaces are still subject to
breaking change between minor versions. After v1.0 the core becomes
semver-stable.

## Building from source

```bash
go build -o scopecache ./cmd/scopecache
go test ./...
```

Module path: `github.com/VeloxCoding/scopecache`. Stdlib only.

## Documentation

The full design, endpoint contracts, and architectural rationale live
in [docs/scopecache-core-rfc.md](docs/scopecache-core-rfc.md).

## License

Apache License, Version 2.0. See [LICENSE](LICENSE).

Copyright 2026 VeloxCoding.
