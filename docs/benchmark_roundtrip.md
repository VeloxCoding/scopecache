# 1. In-Process Cache vs Application-Mediated Redis: HTTP Read Roundtrip Benchmark

Date: 2026-05-08

## 2. Benchmark Purpose

This benchmark is designed to measure the read latency of a simple `GET` request in which Caddy forwards the request to an application runtime, and that runtime uses Redis to fetch data and return the HTTP response. The application layer intentionally performs no business logic and no JSON decoding or re-encoding. The purpose is to measure the practical roundtrip cost of the request path.

The benchmark compares two Redis-backed HTTP read paths: one implemented in Node.js and one implemented in PHP using FrankenPHP worker mode. These routes are then compared with ScopeCache, an in-memory data store that, like Redis, can serve key-based read requests.

The key difference is architectural. Redis is accessed through an application runtime, so each HTTP request must travel through Caddy, the application layer, and Redis before a response can be returned. ScopeCache, by contrast, can be compiled directly into Caddy as a module and runs inside the same process. This allows Caddy to serve cache reads directly from in-process memory, without forwarding the request to Node.js or PHP and without making an external Redis call.

The purpose of the benchmark is to quantify that roundtrip difference and make the latency impact of an in-process, Caddy-integrated data store visible.

Summary including average p50 latency:

| Route | Requests/sec avg | p50 avg |
| --- | ---: | ---: |
| Node raw -> Redis | 30,414 | 1.870ms |
| FrankenPHP worker raw -> Redis | 30,543 | 1.969ms |
| Caddy -> ScopeCache get-seq | 222,554 | 0.187ms |

Node.js and FrankenPHP worker mode are effectively equal in throughput in this benchmark (the difference between is measurement noise). But ScopeCache reaches roughly 7.3x higher throughput in this setup because Caddy can serve the response directly from in-process memory.

ScopeCache is not a general replacement for Redis. Redis offers a much broader feature set, including richer data structures and querying patterns. However, for specific read- or write-heavy use cases where the goal is to return highly dynamic data with the lowest possible latency, ScopeCache can offer advantages by serving responses directly from inside the Caddy process.

ScopeCache is deliberately limited in order to stay small, fast, and robust. Its lookup model is intentionally simple: data is organized by scope, which acts as a namespace, and can be accessed by sequence number or item ID. At the same time, both scope names and item IDs are application-defined strings, which makes the model flexible enough for several real-world use cases. See the full ScopeCache RFC for more information: https://github.com/VeloxCoding/scopecache/blob/main/docs/scopecache-core-rfc.md

For example, the following request returns the 100 latest reactions for topic `43434`:

```text
GET /tail?scope=thread:43434:reactions&limit=100
```

The following request returns one specific reaction:

```text
GET /get?scope=thread:43434:reactions&seq=42
```

This benchmark is not intended to demonstrate all possible ScopeCache use cases. Its purpose is to provide insight into the performance benefits of in-memory caching, and more specifically, of integrating an in-memory data store directly into Caddy.

## 3. What Is Being Measured

The goal is not to benchmark Redis itself. However, in a typical HTTP use case, Redis is not called directly by the client. A request first reaches the web server, is forwarded to an application runtime, the application performs a Redis `GET`, and the application then builds and returns the HTTP response. This full request path is the roundtrip that matters for end-user latency and for the number of HTTP requests the server can handle per second. That is the path measured in this benchmark.

```text
client -> Caddy -> application runtime -> Redis -> application runtime -> Caddy -> client
```

For ScopeCache, the path is shorter:

```text
client -> Caddy/ScopeCache -> client
```

ScopeCache runs as an in-process data store inside the same Caddy binary. There is no separate cache server or external service that data has to be sent to and retrieved from. The cache is directly accessible to Caddy through the compiled module, which removes an entire application/runtime and network roundtrip from the read path.

Measuring that difference and showing the resulting performance benefits is the point of this benchmark.

## 4. Docker and CPU Settings

A self-contained reproduction recipe for the ScopeCache side of this benchmark — Dockerfile, Caddyfile, wrk Lua script, seed script, docker-compose — lives in [benchmark_roundtrip_setup.md](benchmark_roundtrip_setup.md). It works on Linux native and Windows/macOS Docker Desktop (with documented platform caveats).

All three benchmarks run in their own single-container Docker setup. This is deliberate: it keeps container-to-container network latency out of the measurement.

Common settings:

```text
Docker container cpuset:      0-15
wrk process taskset:          0-3
server process taskset:       4-15
wrk shape:                    -t4 -c64 -d5s --latency --timeout 2s
runs:                         10
benchmark dataset:            50,000 items
```

The Docker container itself is allowed to use CPUs `0-15`. Inside the container, `wrk` is pinned to CPUs `0-3`, while the server-side processes are pinned to CPUs `4-15`. This keeps the load generator mostly separated from the system under test.

The Docker container was run on an AMD Ryzen AI Max+ 395 system with 32 GB of LPDDR5X-8000 memory.

Per setup:

| Setup | Container | Host port | Server path |
| --- | --- | ---: | --- |
| Node.js + Redis | `codex-node-redis-bench` | 8090 | Caddy -> Node.js -> Redis |
| FrankenPHP + Redis | `codex-frankenphp-redis-bench` | 8091 | Caddy/FrankenPHP worker -> Redis |
| Caddy + ScopeCache | `codex-caddy-scopecache-bench` | 8092 | Caddy -> ScopeCache module |

Redis runs inside the Node.js and FrankenPHP containers and is accessed through a Unix socket:

```text
/run/redis.sock
```

For both Node.js and PHP, including normal PHP mode and FrankenPHP worker mode, the benchmarks use random reads from a dataset of 50,000 items. The random item ID is generated by the wrk Lua script.


```text
/redis-random-raw?id=<random 1..50000>
/worker-redis-raw?id=<random 1..50000>
```

ScopeCache runs as a Caddy module inside the same Caddy process. The ScopeCache benchmark uses:

```text
/get?scope=bench&seq=<random 1..50000>
```

Both Redis and ScopeCache are populated with a dataset containing exactly 50,000 items.

## 5. Caddy and Application Optimizations

Caddy access logging is not enabled in these benchmarks. There is no request-per-request access log on the hot path.

The Node.js and PHP routes use raw response paths. They do not decode JSON from Redis and do not build a wrapper response. The measured path is:

```text
Redis GET -> JSON string -> direct HTTP response
```

That means the application does not perform:

```text
JSON.parse()
json_decode()
json_encode() wrapper response
duration_us metadata generation
```

This keeps the Redis-backed benchmarks focused on roundtrip cost: Caddy forwarding, application runtime handling, Redis `GET`, and response writing.

ScopeCache avoids the application runtime and Redis call entirely for this read path. The data is already in memory inside the Caddy/ScopeCache process, so Caddy can respond directly.

## 6. Complete Benchmark Results

### 6.1 Node.js Raw -> Redis

Measured path:

```text
wrk -> Caddy -> Node.js -> Redis -> Node.js -> Caddy -> response
```

Benchmark shape:

```text
endpoint: /redis-random-raw
10 runs
5 seconds per run
wrk pinned to CPU 0-3
server processes pinned to CPU 4-15
wrk -t4 -c64 -d5s --latency --timeout 2s
50,000 Redis benchmark items
single Docker container
```

Result:

| Metric | Value |
| --- | ---: |
| Requests/sec avg | 30,414 |
| Latency avg | 2.179ms |
| p50 avg | 1.870ms |
| p90 avg | 3.937ms |
| p95 avg | 4.783ms |
| p99 avg | 6.754ms |
| Errors | 0 |

### 6.2 FrankenPHP Worker Raw -> Redis

Measured path:

```text
wrk -> Caddy/FrankenPHP -> PHP worker -> Redis -> PHP worker -> Caddy/FrankenPHP -> response
```

Benchmark shape:

```text
endpoint: /worker-redis-raw
10 runs
5 seconds per run
wrk pinned to CPU 0-3
server stack pinned to CPU 4-15
wrk -t4 -c64 -d5s --latency --timeout 2s
50,000 Redis benchmark items
single Docker container
```

Result:

| Metric | Value |
| --- | ---: |
| Requests/sec avg | 30,543 |
| Latency avg | 2.069ms |
| p50 avg | 1.969ms |
| p90 avg | 2.631ms |
| p95 avg | 2.981ms |
| p99 avg | 3.686ms |
| Errors | 0 |

### 6.3 Caddy -> ScopeCache Get-Seq

Measured path:

```text
wrk -> Caddy/ScopeCache -> response
```

Benchmark shape:

```text
endpoint: /get?scope=bench&seq=<random 1..50000>
10 runs
5 seconds per run
wrk pinned to CPU 0-3
server process pinned to CPU 4-15
wrk -t4 -c64 -d5s --latency --timeout 2s
50,000 ScopeCache benchmark items
single Docker container
```

Result:

| Metric | Value |
| --- | ---: |
| Requests/sec avg | 222,554 |
| Latency avg | 0.395ms |
| p50 avg | 0.187ms |
| p90 avg | 0.992ms |
| p95 avg | 1.539ms |
| p99 avg | 2.754ms |
| Errors | 0 |

## 7. Interpretation

Node.js raw and FrankenPHP worker raw are effectively equivalent in throughput in this setup. The small difference between them is not large enough to treat as meaningful.

ScopeCache is substantially faster for this read path because it removes two major parts of the Redis-backed route:

```text
application runtime hop
Redis roundtrip
```

Redis itself is extremely fast. In this benchmark, the slower part is not Redis as an in-memory data structure, but the full HTTP request path required to reach Redis through a general-purpose application runtime.

A simple ScopeCache lookup is also extremely fast: a sequential in-process GET lookup takes about 43 ns, or roughly 23 million lookups per second per core. Although that is faster than Redis, the real performance benefit comes from architecture: ScopeCache runs inside the same process as Caddy, so Caddy can answer cache reads directly without crossing into an application runtime and without making a separate Redis roundtrip.
 
## 8. Conclusion

For this simple dynamic read workload, Node.js and FrankenPHP worker mode both deliver roughly 30k requests per second when returning raw JSON from Redis through Caddy.

ScopeCache delivers roughly 222k requests per second in the same benchmark shape because it serves the read directly from Caddy's own process memory.

The core claim is therefore:

```text
Redis is fast, but the route through Caddy -> application runtime -> Redis is longer.
ScopeCache wins here because Caddy can answer directly from in-process memory.
```

ScopeCache is one implementation of a broader idea: building an in-memory datastore directly into Caddy as a module. This benchmark demonstrates the performance value of that architectural approach.