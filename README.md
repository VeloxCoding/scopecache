# scopecache

ScopeCache is a in-memory publish cache and write buffer.

It works standalone over plain HTTP, for example over a Unix socket, from any programming language. But it is designed to shine as a Caddy module: when installed inside Caddy, ScopeCache runs in the same process as the webserver, allowing Caddy to serve ScopeCache data directly from memory.

This avoids the usual request path through separate application and storage processes — such as PHP, Node.js, Redis, or a database — on every request. In benchmark tests, this direct in-process path served data about 7× faster than routing the same request through Node.js and Redis, even though all services were installed on the same server.

That is the core trade-off: ScopeCache is intentionally narrower, but by removing separate application and storage services from the request path, it can reach a latency and throughput profile that traditional multi-service request paths cannot realistically match.

ScopeCache deliberately keeps filtering limited. The core only addresses a few official top-level fields, which keeps it robust, predictable, and fast. But because scope names and IDs are plain strings, ScopeCache remains flexible enough for many real-world use cases.

For example, the following request returns the latest 100 reactions for thread `123`:

```
https://example.com/tail?scope=thread:123&limit=100
```

ScopeCache is not a replacement for Redis or similar in-memory data stores. Those systems offer a much broader feature set: more data types, richer commands, clustering and many other capabilities. They are fast. That is not the debate. The issue is the request path around them. Even on the same server, those extra process and service boundaries add latency and reduce throughput. 

ScopeCache is not an HTTP response cache like Varnish or Souin. It is a scope-addressed publish cache: your application decides what data is stored, under which scope, and when it is updated or removed.

ScopeCache can also act as a write buffer, with built-in support for change notifications so external scripts can drain, process, or persist events elsewhere.


## What it is

Redis is fast. That is not the debate. The issue is the request path around Redis. In a typical setup, every cache read still has to cross multiple process and service boundaries — even when everything runs on the same server:

- webserver
- application layer (Node.js, Python, PHP, etc.)
- Redis
- application layer again
- response


scopecache holds a hot slice of your data in RAM in front of your real
data store. Items live inside *scopes* — what other systems call a
namespace or bucket — and are addressable only by `scope`, `id`, or
`seq`. The entire cache is wipeable and rebuildable from the source of
truth at any time. There is no on-disk state, no TTL, no eviction
policy, no application logic.

Two main use cases:

- **Hot-read cache.** Keep frequently queried fragments in memory so
  they don't hit the database on every request. A fronting proxy
  (Caddy, nginx, apache) can serve cached HTML, JSON, or XML straight
  from `/render` without any application layer in between.
- **Write buffer.** Append high-frequency events (analytics hits, log
  lines, chat messages); a background worker drains the buffer in
  batches via `/tail` + `/delete_up_to`.

The core is intentionally limited to a small set of HTTP endpoints
(read, write, bulk, observe) and three filter axes (`scope`, `id`,
`seq`). No query language, no joins, no payload inspection. Anything
beyond the core — multi-tenant gateways, batch dispatchers, write-only
ingestion, custom auth — is built as separate add-on sub-packages on
top of the core's public Go API.

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
running the cache inside the Caddy process removes the application-
runtime hop and the Redis roundtrip from the read path entirely. A
single in-process `getBySeq` lookup itself takes ~43 ns regardless of
scope size (hash-map, O(1)) — about 23 million lookups per second per
core.

These figures are hardware-dependent. The numbers above were
measured on an AMD Ryzen AI Max+ 395 with 32 GB of LPDDR5X-8000;
the same random-seq /get workload on a 4-vCPU / 8 GB VPS reaches
roughly 90,000 req/s. The point of this comparison is the
**relative gap between the three routes, not the absolute
throughput** — and that gap holds across hardware tiers.

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
