#!/usr/bin/env bash
# bench.sh — per-call latency + throughput for the cgo hot-path
# functions of the scopecache FrankenPHP extension.
#
# Modes:
#   ./bench.sh             default — bench get/tail/append at 54-byte payload
#   ./bench.sh --sweep     bench scopecache_get across payload sizes
#                          (54 B / 256 B / 1 KiB / 4 KiB / 10 KiB)
#
# Per-knob env-var overrides for one-off experiments:
#   ITER (default 100000), WARMUP (5000), TAIL_LIMIT (10), BENCH_PORT (18084).

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BIN="$SCRIPT_DIR/dist/frankenphp"
HOST_PORT="${BENCH_PORT:-18084}"
RUNTIME_IMAGE="${RUNTIME_IMAGE:-dunglas/frankenphp:1.12-php8}"
CONTAINER_NAME="frankenphp-ext-bench"
ITER="${ITER:-100000}"
WARMUP="${WARMUP:-5000}"
TAIL_LIMIT="${TAIL_LIMIT:-10}"

MODE="${1:-default}"
case "$MODE" in
    default|--sweep) ;;
    *) echo "usage: $0 [--sweep]" >&2; exit 2 ;;
esac

if [ ! -f "$BIN" ]; then
    echo "bench: $BIN not found — run ./build.sh first" >&2
    exit 1
fi

cleanup() { docker rm -f "$CONTAINER_NAME" >/dev/null 2>&1 || true; }
trap cleanup EXIT

MSYS_NO_PATHCONV=1 docker run -d --rm \
    --name "$CONTAINER_NAME" \
    -v "$SCRIPT_DIR:/app:ro" \
    -p "$HOST_PORT:8080" \
    --entrypoint /app/dist/frankenphp \
    "$RUNTIME_IMAGE" \
    run --config /app/Caddyfile.bench --adapter caddyfile >/dev/null

for i in $(seq 1 100); do
    if curl -sf --max-time 1 "http://127.0.0.1:$HOST_PORT/stats" >/dev/null 2>&1; then break; fi
    sleep 0.1
done

if [ "$MODE" = "default" ]; then
    curl -sf --max-time 120 \
        "http://127.0.0.1:$HOST_PORT/bench.php?iter=$ITER&warmup=$WARMUP&tail_limit=$TAIL_LIMIT"
    exit 0
fi

# Sweep mode. Iter scales down for larger payloads so wallclock stays
# reasonable; per-call latency grows enough that fewer iters still give
# a clear signal.
echo
printf "%-10s | %-10s | %-12s | %-15s\n" "payload" "iter" "per call" "throughput"
printf "%s\n" "-----------+------------+--------------+----------------"

for pair in \
    "54     200000" \
    "256    200000" \
    "1024   100000" \
    "4096    50000" \
    "10240   20000"
do
    size="${pair%% *}"
    iter="${pair##* }"
    out="$(curl -sf --max-time 120 "http://127.0.0.1:$HOST_PORT/bench.php?payload=$size&iter=$iter" || echo 'FAIL')"
    if echo "$out" | grep -q "^per call"; then
        per="$(echo "$out"  | awk -F': *' '/^per call/   {print $2}')"
        tput="$(echo "$out" | awk -F': *' '/^throughput/ {print $2}')"
        printf "%-10s | %-10s | %-12s | %-15s\n" "${size} B" "$iter" "$per" "$tput"
    else
        printf "%-10s | %-10s | FAIL: %s\n" "${size} B" "$iter" "$(echo "$out" | head -1)"
    fi
done
echo
