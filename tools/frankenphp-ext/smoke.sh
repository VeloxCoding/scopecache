#!/usr/bin/env bash
# smoke.sh — post-build sanity check for the FrankenPHP scopecache
# extension. Runs in 5-10 seconds; exit 0 = the binary boots, the
# Caddy module wires up, the shared *Gateway is reachable from PHP.
#
# What it verifies, end-to-end:
#
#   1. dist/frankenphp boots under Caddyfile.bench (Caddy module's
#      Provision runs; the *Gateway is registered under "default").
#   2. POST /append goes through the scopecache caddymodule's gateway.
#   3. scopecache_get('demo', 'hello') returns the bytes just written
#      — proves the extension sees the SAME *Gateway, not a private one.
#   4. scopecache_get on an unknown id within a known scope returns NULL.
#   5. scopecache_get on an unknown scope returns NULL.
#
# Exit code: 0 = pass; 1 = fail with diagnostic + server log on stderr.
#
# Runs the binary inside the stock dunglas/frankenphp:1.12-php8
# runtime image so we don't need a Linux host — dist/frankenphp is
# a Linux ELF built inside the matching builder image. Windows/macOS
# users get the same smoke check as Linux users.
#
# Port: 18080 on the host (avoiding the common 8080 default).
#
# Usage:
#   ./smoke.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BIN="$SCRIPT_DIR/dist/frankenphp"
HOST_PORT="${SMOKE_PORT:-18080}"
RUNTIME_IMAGE="${SMOKE_IMAGE:-dunglas/frankenphp:1.12-php8}"
CONTAINER_NAME="frankenphp-ext-smoke"

if [ ! -f "$BIN" ]; then
    # Use -f not -x: on Windows NTFS the host can't see the +x bit
    # but the Linux container can.
    echo "smoke: $BIN not found — run ./build.sh first" >&2
    exit 1
fi

cleanup() {
    docker rm -f "$CONTAINER_NAME" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# Start the binary inside the runtime image; mount this scripts dir
# as /app so Caddyfile.bench's `root * /app` resolves test.php +
# dist/frankenphp.
echo ">>> starting $CONTAINER_NAME on host port $HOST_PORT"
MSYS_NO_PATHCONV=1 docker run -d --rm \
    --name "$CONTAINER_NAME" \
    -v "$SCRIPT_DIR:/app:ro" \
    -p "$HOST_PORT:8080" \
    --entrypoint /app/dist/frankenphp \
    "$RUNTIME_IMAGE" \
    run --config /app/Caddyfile.bench --adapter caddyfile >/dev/null

# Wait up to ~10s for the listener to bind (cold container start +
# Caddy module Provision can take a moment on first run).
ready=0
for i in $(seq 1 100); do
    if curl -sf --max-time 1 "http://127.0.0.1:$HOST_PORT/stats" >/dev/null 2>&1; then
        ready=1
        break
    fi
    sleep 0.1
done

if [ "$ready" -ne 1 ]; then
    echo "smoke: server failed to start within 10s — container log:" >&2
    docker logs "$CONTAINER_NAME" 2>&1 | tail -30 >&2
    exit 1
fi

# Hit test.php — this both seeds via /append and exercises the extension.
OUT="$(curl -sf --max-time 5 "http://127.0.0.1:$HOST_PORT/test.php" || true)"

if [ -z "$OUT" ]; then
    echo "smoke: /test.php returned no body — container log:" >&2
    docker logs "$CONTAINER_NAME" 2>&1 | tail -30 >&2
    exit 1
fi

# Validate the expected outcomes. test.php uses var_dump, so a hit on
# scopecache_get prints `string(N) "..."` and misses print `NULL`.
# scopecache_append prints `seq=<n>`. scopecache_tail prints either
# `array(N) {...}` (hit) or `NULL` (miss).
fail=0

# --- scopecache_get -----------------------------------------------------------

if echo "$OUT" | grep -A 3 "scopecache_get('demo', 'hello') ->" | grep -qE '\["hit"\]=>'$'\n''[[:space:]]*bool\(true\)'; then
    echo "  PASS  get hit: envelope shows hit=true"
else
    echo "  FAIL  get hit: envelope did NOT show hit=true" >&2
    fail=1
fi

if echo "$OUT" | grep -A 3 "scopecache_get('demo', 'no-such-item') ->" | grep -qE '\["hit"\]=>'$'\n''[[:space:]]*bool\(false\)'; then
    echo "  PASS  get miss (unknown id): envelope shows hit=false"
else
    echo "  FAIL  get miss (unknown id): expected hit=false" >&2
    fail=1
fi

if echo "$OUT" | grep -A 3 "scopecache_get('no-such-scope', 'hello') ->" | grep -qE '\["hit"\]=>'$'\n''[[:space:]]*bool\(false\)'; then
    echo "  PASS  get miss (unknown scope): envelope shows hit=false"
else
    echo "  FAIL  get miss (unknown scope): expected hit=false" >&2
    fail=1
fi

# --- scopecache_append --------------------------------------------------------
#
# /append envelope: ok=true, created=true, item.seq>=1. We assert
# created=true on the line beneath the call header (3-line lookahead).

if echo "$OUT" | grep -A 4 "scopecache_append('demo'," | grep -qE '\["created"\]=>'$'\n''[[:space:]]*bool\(true\)'; then
    echo "  PASS  append into existing scope: created=true"
else
    echo "  FAIL  append into 'demo': expected created=true" >&2
    fail=1
fi

# The just-appended item must be readable back: the second
# scopecache_get-by-php-write-* call header (the read-back) must
# carry an envelope with hit=true.
if echo "$OUT" | grep -A 4 "scopecache_get('demo', 'php-write-" | grep -qE '\["hit"\]=>'$'\n''[[:space:]]*bool\(true\)'; then
    echo "  PASS  append read-back: get envelope hit=true on just-written item"
else
    echo "  FAIL  append read-back: get did NOT see just-written item" >&2
    fail=1
fi

# Bootstrap: appending to a never-seen scope creates it.
if echo "$OUT" | grep -A 4 "scopecache_append('php-side-scope'," | grep -qE '\["created"\]=>'$'\n''[[:space:]]*bool\(true\)'; then
    echo "  PASS  append into fresh scope: created=true"
else
    echo "  FAIL  append into 'php-side-scope': expected created=true" >&2
    fail=1
fi

# --- scopecache_tail ----------------------------------------------------------
#
# /tail envelope on hit: hit=true + items[] non-empty. On miss:
# hit=false + items=[] (no longer PHP null — matches HTTP /tail).

if echo "$OUT" | grep -A 3 "scopecache_tail('demo', 5) ->" | grep -qE '\["hit"\]=>'$'\n''[[:space:]]*bool\(true\)'; then
    echo "  PASS  tail hit: envelope shows hit=true"
else
    echo "  FAIL  tail hit: scopecache_tail('demo', 5) did NOT show hit=true" >&2
    fail=1
fi

if echo "$OUT" | grep -A 3 "scopecache_tail('no-such-scope', 5) ->" | grep -qE '\["hit"\]=>'$'\n''[[:space:]]*bool\(false\)'; then
    echo "  PASS  tail miss (unknown scope): envelope shows hit=false"
else
    echo "  FAIL  tail miss (unknown scope): expected hit=false" >&2
    fail=1
fi

if [ "$fail" -ne 0 ]; then
    echo >&2
    echo "smoke: FAIL — full /test.php response:" >&2
    echo "----" >&2
    echo "$OUT" >&2
    echo "----" >&2
    echo "container log:" >&2
    docker logs "$CONTAINER_NAME" 2>&1 | tail -20 >&2
    exit 1
fi

echo "smoke: PASS"
