# Install Caddy + scopecache on a fresh VPS

One line on a fresh Ubuntu/Debian VPS:

```bash
curl -fsSL https://raw.githubusercontent.com/VeloxCoding/scopecache/main/scripts/install_caddyscope.sh | sudo bash
```

When that finishes:

- Caddy + scopecache is running on `:80`.
- The systemd unit `caddy.service` auto-starts on reboot and restarts
  on crash.
- `/help` has been smoke-tested.
- `wrk` is installed (for the benchmark step below).

The script does **not** run `apt upgrade` — that's the operator's
decision, not the installer's. Run `sudo apt upgrade` separately if
you want a full system upgrade first.

## Configuration

All optional. Pass env vars before `bash` to override defaults:

```bash
# Different port
curl -fsSL https://raw.githubusercontent.com/VeloxCoding/scopecache/main/scripts/install_caddyscope.sh | sudo PORT=8080 bash

# Pin a specific release (instead of latest)
curl -fsSL https://raw.githubusercontent.com/VeloxCoding/scopecache/main/scripts/install_caddyscope.sh | sudo VERSION=v0.8.18 bash

# Larger capacity caps
curl -fsSL https://raw.githubusercontent.com/VeloxCoding/scopecache/main/scripts/install_caddyscope.sh | sudo MAX_STORE_MB=1024 SCOPE_MAX_ITEMS=1000000 bash

# Combined
curl -fsSL https://raw.githubusercontent.com/VeloxCoding/scopecache/main/scripts/install_caddyscope.sh | sudo VERSION=v0.8.18 PORT=8080 MAX_STORE_MB=500 bash
```

Available env vars:

| Var               | Default     | Meaning                                    |
|-------------------|-------------|--------------------------------------------|
| `VERSION`         | `latest`    | Release tag (e.g. `v0.8.18`)               |
| `PORT`            | `80`        | TCP port Caddy listens on                  |
| `SCOPE_MAX_ITEMS` | `100000`    | Per-scope item cap                         |
| `MAX_STORE_MB`    | `100`       | Store-wide byte cap (MiB)                  |
| `MAX_ITEM_MB`     | `1`         | Per-item byte cap (MiB)                    |

## Benchmark

A separate one-liner runs a load test against the cache. No `sudo` —
the benchmark only fires HTTP requests, it doesn't touch the system:

```bash
curl -fsSL https://raw.githubusercontent.com/VeloxCoding/scopecache/main/scripts/run_benchmark.sh | bash
```

What it does:

1. Fills scope `benchmark` with 50,000 small items via one bulk
   `/warm` request (typically <100 ms).
2. Runs a 5-second `wrk` workload against
   `/get?scope=benchmark&seq=<random>` and reports req/sec plus
   latency p50, p95, max.

### Benchmark tuning

Same pattern — env vars before `bash`:

```bash
# Heavier load (60 seconds, 200 concurrent connections)
curl -fsSL https://raw.githubusercontent.com/VeloxCoding/scopecache/main/scripts/run_benchmark.sh | WRK_DURATION=60s WRK_CONNECTIONS=200 bash

# Skip the fill step, only run wrk against an already-populated scope
curl -fsSL https://raw.githubusercontent.com/VeloxCoding/scopecache/main/scripts/run_benchmark.sh | STEPS=2 bash

# Hit a remote VPS from your laptop instead of localhost
curl -fsSL https://raw.githubusercontent.com/VeloxCoding/scopecache/main/scripts/run_benchmark.sh | URL=http://1.2.3.4 bash
```

Available env vars:

| Var               | Default            | Meaning                                              |
|-------------------|--------------------|------------------------------------------------------|
| `URL`             | `http://localhost` | Base URL of the cache (no trailing slash)            |
| `COUNT`           | `50000`            | Items to insert in step 1                            |
| `SCOPE`           | `benchmark`        | Scope name to write into / read from                 |
| `WRK_THREADS`     | `1`                | wrk worker threads                                   |
| `WRK_CONNECTIONS` | `50`               | wrk concurrent connections                           |
| `WRK_DURATION`    | `5s`               | wrk run duration                                     |
| `STEPS`           | `1,2`              | Comma-separated step list (e.g. `STEPS=2`)           |
