#!/usr/bin/env bash
# payload-sweep.sh — measures scopecache_get per-call cost across
# payload sizes for the current dist/frankenphp binary.
#
# Reveals where the per-call cost shifts from cgo-boundary-dominated
# (small payloads) to memcpy-dominated (large payloads). Use to
# decide whether Option 4 in CLAUDE_PHPEXTENSION_IN_GO.md (Gateway-
# clone bypass, ~one less payload-size memcpy per call) is worth
# the core-API expansion.
#
# Same container start/stop pattern as smoke.sh / compare.sh.
# Default port 18082 to avoid conflicts with smoke/compare.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
HOST_PORT="${SWEEP_PORT:-18082}"
RUNTIME_IMAGE="${RUNTIME_IMAGE:-dunglas/frankenphp:1.12-php8}"
BIN_IN_CONTAINER="${BIN_IN_CONTAINER:-/app/dist/frankenphp}"
CONTAINER_NAME="scopecache-payload-sweep"

# (size_in_bytes, iter) pairs. Iter scales down for larger payloads
# so the bench finishes in reasonable wallclock without losing
# statistical resolution (per-call latency grows fast enough that
# fewer iters still give a clear signal).
SIZES_ITER=(
    "54     200000"
    "256    200000"
    "1024   100000"
    "4096   50000"
    "10240  20000"
)

cleanup() { docker rm -f "$CONTAINER_NAME" >/dev/null 2>&1 || true; }
trap cleanup EXIT

host_path="$SCRIPT_DIR${BIN_IN_CONTAINER#/app}"
if [ ! -f "$host_path" ]; then
    echo "payload-sweep: $host_path not found — run ./build.sh first" >&2
    exit 1
fi

echo ">>> starting $CONTAINER_NAME on host port $HOST_PORT ($BIN_IN_CONTAINER)"
MSYS_NO_PATHCONV=1 docker run -d --rm \
    --name "$CONTAINER_NAME" \
    -v "$SCRIPT_DIR:/app:ro" \
    -p "$HOST_PORT:8080" \
    --entrypoint "$BIN_IN_CONTAINER" \
    "$RUNTIME_IMAGE" \
    run --config /app/Caddyfile.bench --adapter caddyfile >/dev/null

# Wait until ready.
ready=0
for i in $(seq 1 100); do
    if curl -sf --max-time 1 "http://127.0.0.1:$HOST_PORT/stats" >/dev/null 2>&1; then
        ready=1; break
    fi
    sleep 0.1
done
if [ "$ready" -ne 1 ]; then
    echo "payload-sweep: server failed to start within 10s" >&2
    docker logs "$CONTAINER_NAME" 2>&1 | tail -20 >&2
    exit 1
fi

# Header.
printf "\n%-10s | %-10s | %-12s | %-15s\n" "payload" "iter" "per call" "throughput"
printf "%s\n" "-----------+------------+--------------+----------------"

for pair in "${SIZES_ITER[@]}"; do
    size="${pair%% *}"
    iter="${pair##* }"
    out="$(curl -sf --max-time 120 "http://127.0.0.1:$HOST_PORT/bench-ext-only.php?payload=$size&iter=$iter" || echo 'FAIL')"
    if echo "$out" | grep -q "^per call"; then
        per="$(echo "$out" | awk -F': *' '/^per call/ {print $2}')"
        tput="$(echo "$out" | awk -F': *' '/^throughput/ {print $2}')"
        printf "%-10s | %-10s | %-12s | %-15s\n" "${size} B" "$iter" "$per" "$tput"
    else
        printf "%-10s | %-10s | FAIL: %s\n" "${size} B" "$iter" "$(echo "$out" | head -1)"
    fi
done
echo
