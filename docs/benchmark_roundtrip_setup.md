# Caddy + ScopeCache Roundtrip Benchmark — Setup Guide

This document is a self-contained recipe for reproducing the ScopeCache
side of [docs/benchmark_roundtrip.md](benchmark_roundtrip.md). Works on
both Linux (native Docker) and Windows/macOS (Docker Desktop). You do
not need to clone the scopecache repository — the Caddy module is
fetched from GitHub by `xcaddy` during the image build.

**Requirements:**

- Docker (any recent version, native on Linux or Docker Desktop on
  Windows/macOS)
- ≥16 CPU cores allocated to the container's `cpuset` (see §2)
- ~2 GB free memory
- Network access to `proxy.golang.org` and `github.com` during the
  image build (the Caddy module is fetched at build time)

**Platform notes that affect the numbers:**

- **Linux native** — closest reproduction of the published results.
  The Linux scheduler honors `cpuset`/`taskset` exactly, no VM
  layer.
- **Windows + Docker Desktop (WSL2 backend)** — works, but the
  container runs inside a WSL2 Linux VM. The WSL2 kernel honors
  `cpuset` inside the VM, but the Windows host scheduler can still
  move the VM's vCPUs across physical cores. Expect 30–60% lower
  throughput than Linux-native on the same hardware. Tail-latency
  (p99) is typically the most affected metric.
- **macOS + Docker Desktop (Apple Silicon or Intel)** — similar
  caveat: containers run inside a lightweight VM. Numbers will not
  match native-Linux results but the *relative* shape (in-process
  cache faster than the Redis-backed routes) holds.

The shell-snippets below are split by platform where they differ.
Where a command works identically on Linux/macOS bash/zsh and
Windows PowerShell, only one form is shown.

## 1. Goal

Run Caddy with the ScopeCache Caddy module compiled in, so a `GET`
request can be answered directly from in-process memory. Hit it with
`wrk` and capture latency + requests-per-second.

The benchmark endpoint is:

```text
/get?scope=bench&seq=<random 1..50000>
```

Both Caddy and `wrk` run inside the same Docker container so
container-to-container or host-network latency stays out of the
measurement.

## 2. Docker layout

One container, pinned to half a 16-core machine:

| Setting | Value |
|---|---|
| Container CPU set | `0-15` |
| Server (Caddy) taskset | `4-15` |
| `wrk` taskset | `0-3` |
| Dataset | 50,000 items |
| Host port | `8092` (any free port works) |
| Container port | `80` |

Pinning `wrk` and the server on disjoint cores keeps load-generator
contention out of the server's CPU budget. On a host with fewer cores,
shrink both ranges proportionally — the only hard rule is that the
two cpusets must not overlap.

## 3. Files to create

Create an empty folder anywhere on your machine and put the five
files below inside it. The folder name is irrelevant; this guide
calls it `<bench-folder>`.

```text
<bench-folder>/
├── Dockerfile
├── Caddyfile
├── wrk-get-seq.lua
├── start.sh
└── docker-compose.yml
```

### 3.1 `Dockerfile`

```dockerfile
FROM caddy:2-builder AS builder

RUN xcaddy build \
    --with github.com/VeloxCoding/scopecache/caddymodule@latest

FROM caddy:2-alpine

RUN apk add --no-cache curl wrk util-linux

WORKDIR /app

COPY --from=builder /usr/bin/caddy /usr/bin/caddy
COPY Caddyfile /etc/caddy/Caddyfile
COPY wrk-get-seq.lua /app/wrk-get-seq.lua
COPY start.sh /usr/local/bin/caddy-scopecache-start

RUN chmod +x /usr/local/bin/caddy-scopecache-start

ENV BENCH_ITEMS=50000
ENV SERVER_CPUSET=4-15
ENV WRK_CPUSET=0-3

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD curl -fsS "http://127.0.0.1:80/get?scope=bench&seq=1" >/dev/null || exit 1

CMD ["/usr/local/bin/caddy-scopecache-start"]
```

The first stage runs `xcaddy build --with
github.com/VeloxCoding/scopecache/caddymodule@latest`. This pulls
the ScopeCache module from GitHub and compiles it into a single
Caddy binary — no separate cache process, no Redis, no application
runtime.

To pin a specific scopecache version, replace `@latest` with the tag
you want, for example `@v0.8.16`.

### 3.2 `Caddyfile`

```caddyfile
{
    admin off
    auto_https off
}

:80 {
    handle / {
        header Content-Type "text/html; charset=utf-8"
        respond <<HTML
<!doctype html>
<html lang="en">
<head><meta charset="utf-8"><title>ScopeCache Bench</title></head>
<body>
  <h1>Caddy -> ScopeCache</h1>
  <p><a href="/get?scope=bench&amp;seq=12345">GET random-seeded item by seq</a></p>
  <p><a href="/help">ScopeCache help</a></p>
</body>
</html>
HTML 200
    }

    scopecache {
        scope_max_items 10000000
        max_store_mb 8192
        max_item_mb 16
        events_mode off
    }
}
```

Settings that matter for the benchmark:

- `scope_max_items 10000000` — well above the 50,000-item dataset.
- `max_store_mb 8192` — store cap with headroom for larger experiments.
- `max_item_mb 16` — per-item cap.
- `events_mode off` — `_events` auto-populate disabled, so the read
  path is what gets measured.

`admin off` and `auto_https off` keep Caddy single-purpose for a
local HTTP-only benchmark.

### 3.3 `wrk-get-seq.lua`

```lua
local thread_count = 0

function setup(thread)
  thread:set("tid", thread_count)
  thread_count = thread_count + 1
end

function init(args)
  max_items = tonumber(os.getenv("BENCH_ITEMS") or "50000")
  math.randomseed(os.time() + (tid or 0) * 1000)
  bad_status = {}
end

function request()
  local seq = math.random(1, max_items)
  return wrk.format("GET", "/get?scope=bench&seq=" .. seq)
end

function response(status)
  if status ~= 200 then
    bad_status[status] = (bad_status[status] or 0) + 1
  end
end

function done(summary, latency, requests)
  io.write(string.format("P95_US: %.0f\n", latency:percentile(95.0)))
  if bad_status == nil then
    return
  end
  for status, count in pairs(bad_status) do
    io.stderr:write("BADSTATUS " .. status .. " " .. count .. "\n")
  end
end
```

The `seq` is generated inside `wrk` (one of `1..BENCH_ITEMS`), so the
server's CPU budget isn't spent generating randomness. Each `wrk`
thread seeds its own RNG so threads don't march in lock-step.

### 3.4 `start.sh`

```sh
#!/bin/sh
set -eu

# 1. Start Caddy pinned to SERVER_CPUSET. Background, capture PID.
taskset -c "$SERVER_CPUSET" caddy run \
    --config /etc/caddy/Caddyfile --adapter caddyfile &
CADDY_PID=$!

# 2. Wait for the cache help endpoint (proves the module loaded).
for _ in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20; do
    if curl -fsS http://127.0.0.1:80/help >/dev/null 2>&1; then
        break
    fi
    sleep 0.5
done
curl -fsS http://127.0.0.1:80/help >/dev/null

# 3. Wipe any pre-existing state.
curl -fsS -X POST http://127.0.0.1:80/wipe >/dev/null

# 4. Build a single rebuild JSON document with $BENCH_ITEMS items.
seed_file="/tmp/scopecache-rebuild.json"
printf '{"items":[' > "$seed_file"
i=1
while [ "$i" -le "$BENCH_ITEMS" ]; do
    if [ "$i" -gt 1 ]; then
        printf ',' >> "$seed_file"
    fi
    printf '{"scope":"bench","id":"item-%s","payload":{"id":%s,"message":"hello from cache","version":1}}' \
        "$i" "$i" >> "$seed_file"
    i=$((i + 1))
done
printf ']}' >> "$seed_file"

# 5. Load the dataset in one /rebuild call.
curl -fsS -X POST -H 'Content-Type: application/json' \
    --data-binary "@$seed_file" \
    http://127.0.0.1:80/rebuild >/tmp/scopecache-seed.log

# 6. Verify the cache reports the expected item count.
curl -fsS http://127.0.0.1:80/stats | grep -q "\"total_items\":$BENCH_ITEMS" \
    || { echo "seed verification failed"; exit 1; }

echo "scopecache ready: $BENCH_ITEMS items in scope=bench"

# 7. Block on Caddy so the container stays alive for `docker exec`.
wait "$CADDY_PID"
```

The seed runs once at container start, in a single `/rebuild` call.
That keeps startup deterministic and avoids turning the seed phase
itself into a benchmark.

### 3.5 `docker-compose.yml`

```yaml
services:
  bench:
    build:
      context: .
      dockerfile: Dockerfile
    image: caddy-scopecache-bench
    container_name: caddy-scopecache-bench
    cpuset: "0-15"
    ports:
      - "8092:80"
    environment:
      BENCH_ITEMS: "50000"
      SERVER_CPUSET: "4-15"
      WRK_CPUSET: "0-3"
```

The `cpuset: "0-15"` line is what isolates the benchmark to half the
host's cores. Adjust the range to match your hardware. The two
`*_CPUSET` env-vars are read by `start.sh` (server) and the wrk
command shown below (load generator).

## 4. Build and start

Run from the folder containing the five files above. The `docker`
commands are identical on Linux, macOS, and Windows.

```sh
docker compose -p caddy-scopecache-bench up --build -d
```

Verify it came up:

```sh
docker ps --filter name=caddy-scopecache-bench
docker logs caddy-scopecache-bench | tail -20
```

You should see `scopecache ready: 50000 items in scope=bench` in the
log. If the seed verification line is missing, the cache loaded but
seeding did not complete — run `docker compose logs bench` to inspect.

A quick sanity check from the host:

**Linux / macOS:**
```sh
curl -fsS "http://127.0.0.1:8092/get?scope=bench&seq=1"
```

**Windows PowerShell:** PowerShell aliases `curl` to
`Invoke-WebRequest`, which has different flags. Use one of:

```powershell
curl.exe -fsS "http://127.0.0.1:8092/get?scope=bench&seq=1"
```

```powershell
Invoke-WebRequest -UseBasicParsing "http://127.0.0.1:8092/get?scope=bench&seq=1" |
    Select-Object -ExpandProperty Content
```

Or — platform-agnostic — run the curl from inside the container:

```sh
docker exec caddy-scopecache-bench curl -fsS "http://127.0.0.1:80/get?scope=bench&seq=1"
```

This should return one item with `id=item-1` and the seeded payload.

## 5. Run the benchmark

`wrk` is installed inside the container; run it through `docker exec`
so it inherits the `cpuset` constraints and `taskset`s onto cores 0-3.

**Linux / macOS:**
```sh
docker exec caddy-scopecache-bench sh -c \
  'taskset -c "$WRK_CPUSET" wrk -t4 -c64 -d5s --latency --timeout 2s \
   -s /app/wrk-get-seq.lua http://127.0.0.1:80'
```

**Windows PowerShell:** the single-quoted `sh -c '...'` payload survives
unchanged inside double-quoted PowerShell. Either form works:

```powershell
docker exec caddy-scopecache-bench sh -c "taskset -c `"$env:WRK_CPUSET`" wrk -t4 -c64 -d5s --latency --timeout 2s -s /app/wrk-get-seq.lua http://127.0.0.1:80"
```

Or — simpler — call `sh` with a here-string-friendly inline command:

```powershell
docker exec caddy-scopecache-bench sh -c 'taskset -c "$WRK_CPUSET" wrk -t4 -c64 -d5s --latency --timeout 2s -s /app/wrk-get-seq.lua http://127.0.0.1:80'
```

The single-quoted payload is parsed by `sh` *inside* the container, so
`$WRK_CPUSET` resolves there (where the env-var lives) rather than in
the host shell.

Flag-by-flag:

| Flag | Meaning |
|---|---|
| `taskset -c $WRK_CPUSET` | pin `wrk` to CPUs 0-3 (set by env) |
| `-t4` | 4 `wrk` threads |
| `-c64` | 64 open keepalive connections |
| `-d5s` | run for 5 seconds |
| `--latency` | print full latency distribution |
| `--timeout 2s` | abandon a request after 2 s |
| `-s /app/wrk-get-seq.lua` | the random `seq` Lua script |
| `http://127.0.0.1:80` | Caddy/ScopeCache inside the same container |

A typical run looks like this on a 32-core AMD Ryzen-class host (the
numbers in [docs/benchmark_roundtrip.md](benchmark_roundtrip.md) are
the average of 10 such runs):

```text
Running 5s test @ http://127.0.0.1:80
  4 threads and 64 connections
  Thread Stats   Avg      Stdev     Max   +/- Stdev
    Latency     0.30ms  ...
    Req/Sec    55.6k    ...
  Latency Distribution
     50%    0.187ms
     75%    0.380ms
     90%    0.992ms
     95%    1.539ms
     99%    2.754ms
  ~ 1,100,000 requests in 5.00s, ...MB read
Requests/sec: 222,554
```

For a stable picture run the command 10 times and average. Most
hosts show ±5% variance run-to-run; persistent outliers usually mean
the host is not idle (background CPU work, thermal throttling, etc.).

## 6. Tear down

```sh
docker compose -p caddy-scopecache-bench down
```

The image stays cached locally; the next `up --build` rebuilds only
if `Dockerfile`, `Caddyfile`, the Lua script, or `start.sh` changed.

## 7. Adapting the setup

- **Different scopecache version.** Replace `@latest` in the
  `Dockerfile` with `@vX.Y.Z`.
- **Different dataset size.** Change `BENCH_ITEMS` in
  `docker-compose.yml`. The seed file scales linearly; at 1 M items
  expect ~30 seconds extra startup time.
- **Different load shape.** Tune `-t`, `-c`, `-d` in the `wrk`
  command. The roundtrip benchmark uses `-t4 -c64 -d5s` to keep the
  server below saturation; raising `-c` past ~250 on this host
  starts moving p99 sharply without changing average throughput.
- **Different host shape.** Adjust the cpuset trio (container,
  server, `wrk`). Keep server and `wrk` disjoint; never let them
  overlap.

## 8. Troubleshooting

**`xcaddy build` fails with module-not-found.** Check connectivity to
proxy.golang.org. The `@latest` resolver needs network access during
the image build. Behind a corporate proxy, set `GOPROXY` in the
builder stage:

```dockerfile
FROM caddy:2-builder AS builder
ENV GOPROXY=https://proxy.golang.org,direct
RUN xcaddy build \
    --with github.com/VeloxCoding/scopecache/caddymodule@latest
```

**`docker exec ... taskset` fails with "Operation not permitted".**
The container needs the `SYS_NICE` capability for `taskset`. Most
Docker setups grant this by default; if you stripped capabilities,
add `cap_add: [SYS_NICE]` in `docker-compose.yml`. On Docker Desktop
(Windows/macOS) this capability is granted by default to the Linux
VM the container runs in.

**Throughput on Windows/macOS Docker Desktop is ~half of the
documented numbers.** Expected — the container runs inside a Linux
VM (WSL2 on Windows, lightweight VM on macOS). The host scheduler
moves vCPUs across physical cores, so the container's `cpuset`
isolation is partly virtual. The *relative* shape (in-process
faster than network-hop) still holds, but absolute req/sec and tail
latency will not match a Linux-native run on the same metal. To
reproduce the published Linux numbers, use a Linux-native host or a
cloud VM with native-Linux Docker.

**Throughput is half of what's documented.** Most likely cause: the
server is sharing cores with `wrk` or with another container.
Verify with `docker inspect caddy-scopecache-bench | grep -i cpuset`
and re-check the two `taskset` commands.

**`/help` works but `/get` returns 404.** The seed step did not
complete. Run `docker logs caddy-scopecache-bench | grep -i seed` to
see the failure, or re-run the seed manually:

```sh
docker exec caddy-scopecache-bench sh -c \
  'curl -fsS http://127.0.0.1:80/stats | head -c 200'
```

If `total_items` is `0`, run `start.sh` manually inside the container.
