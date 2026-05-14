#!/usr/bin/env bash
# bench-experimental.sh — A/B benchmarks for the scopecache_x_*
# experimental cgo entry-points (defined in
# addons/frankenphp-ext/scopecache_ext_experimental.go, exposed only
# in builds that include that file — i.e. tools/frankenphp-ext, NOT
# tools/frankenphp-bin which deletes it before frankenphp-gen runs).
#
# Per-knob env-var overrides:
#   ITER (default 100000), WARMUP (5000), BENCH_PORT (18084).

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BIN="$SCRIPT_DIR/dist/frankenphp"
HOST_PORT="${BENCH_PORT:-18084}"
RUNTIME_IMAGE="${RUNTIME_IMAGE:-dunglas/frankenphp:1.12-php8}"
CONTAINER_NAME="frankenphp-ext-bench-experimental"
ITER="${ITER:-100000}"
WARMUP="${WARMUP:-5000}"

if [ ! -f "$BIN" ]; then
    echo "bench-experimental: $BIN not found — run ./build.sh first" >&2
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

curl -sf --max-time 120 \
    "http://127.0.0.1:$HOST_PORT/bench-experimental.php?iter=$ITER&warmup=$WARMUP"
