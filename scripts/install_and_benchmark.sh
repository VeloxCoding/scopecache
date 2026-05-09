#!/usr/bin/env bash
# install_and_benchmark.sh — populate and smoke-benchmark a running
# scopecache instance from any client host.
#
# Steps currently implemented:
#   1. Fill a scope with N small items via a single /warm bulk request.
#   2. Random-seq read benchmark on /get?scope=<S>&seq=<n> via wrk.
#
# Step 1 measures bulk-decode + buffer-append throughput; each item is
# a tiny JSON object (`{"i":<n>}`) so payload encoding is negligible.
# /warm replaces the target scope only; other scopes are left alone.
# (Use /rebuild if you want to atomically wipe the entire cache
# instead — same request shape, different endpoint.)
#
# Step 2 fires a wrk get-seq workload at the cache, picking a random
# seq in the actual range that step 1 just produced (auto-detected via
# /head + /tail because seq "never rewinds" per scope, so it can be
# 1..N or 50001..100000 depending on history). Requires `wrk`; on
# Ubuntu/Debian: `sudo apt install -y wrk`.
#
# Configurable via env vars (defaults shown):
#
#   URL=http://localhost     base URL of the cache (no trailing slash)
#   COUNT=50000              how many items to insert in step 1
#   SCOPE=benchmark          scope name to write into / read from
#   WRK_THREADS=2            wrk worker threads in step 2
#   WRK_CONNECTIONS=50       wrk concurrent connections in step 2
#   WRK_DURATION=10s         wrk run duration in step 2
#   STEPS=1,2                comma-separated step list (e.g. STEPS=2 to
#                            skip the fill and only run wrk against an
#                            already-populated scope)
#
# Examples:
#
#   ./install_and_benchmark.sh                          # local, 50k items + bench
#   URL=http://1.2.3.4 ./install_and_benchmark.sh       # against remote VPS
#   STEPS=2 ./install_and_benchmark.sh                  # bench only
#   WRK_DURATION=30s WRK_CONNECTIONS=200 ./install_and_benchmark.sh
#
# Runs anywhere with curl + awk + (for step 2) wrk. Linux, WSL, the
# dev container. macOS has BSD date so the millisecond timing in step
# 1 falls back to second-only — items still land correctly.

set -euo pipefail

URL="${URL:-http://localhost}"
COUNT="${COUNT:-50000}"
SCOPE="${SCOPE:-benchmark}"
WRK_THREADS="${WRK_THREADS:-2}"
WRK_CONNECTIONS="${WRK_CONNECTIONS:-50}"
WRK_DURATION="${WRK_DURATION:-10s}"
STEPS="${STEPS:-1,2}"

step_enabled() {
    case ",$STEPS," in
        *",$1,"*) return 0 ;;
        *)        return 1 ;;
    esac
}

# GNU date supports %s%N (nanoseconds since epoch); BSD date does not.
# Fall back to second-resolution if %N is unsupported.
if date +%N | grep -q '^[0-9]\{9\}$'; then
    NOW_NS() { date +%s%N; }
else
    NOW_NS() { echo $(($(date +%s) * 1000000000)); }
fi
NS_PER_MS=1000000

# --- step 1: fill --------------------------------------------------

if step_enabled 1; then
    echo "step 1: filling ${URL}/warm — scope=${SCOPE}, count=${COUNT} (single bulk request)"
    start_ns=$(NOW_NS)

    # Stream a single JSON document of the form:
    #
    #   {"items":[{"scope":"<S>","payload":{"i":1}}, ..., {"scope":"<S>","payload":{"i":N}}]}
    #
    # directly into curl's stdin via --data-binary @-. No temp file,
    # no shell-loop overhead — awk emits the whole array in one pass.
    {
        printf '{"items":['
        seq 1 "$COUNT" | awk -v scope="$SCOPE" '
            BEGIN { sep = "" }
            {
                printf "%s{\"scope\":\"%s\",\"payload\":{\"i\":%d}}", sep, scope, $1
                sep = ","
            }
        '
        printf ']}'
    } | curl -fsS -X POST "$URL/warm" \
        -H 'Content-Type: application/json' \
        --data-binary @- \
        -o /dev/null

    end_ns=$(NOW_NS)
    elapsed_ms=$(( (end_ns - start_ns) / NS_PER_MS ))
    [ "$elapsed_ms" -eq 0 ] && elapsed_ms=1
    rate=$(( COUNT * 1000 / elapsed_ms ))
    echo "  done in ${elapsed_ms}ms (~${rate} items/s)"
    echo
fi

# --- step 2: wrk get-seq -------------------------------------------

if step_enabled 2; then
    if ! command -v wrk >/dev/null 2>&1; then
        echo "step 2 needs wrk. On Ubuntu/Debian: sudo apt install -y wrk" >&2
        exit 1
    fi

    # Detect the actual seq range present in the scope. /head returns
    # the oldest items (lowest seqs), /tail the newest (highest seq).
    # Both are JSON; pull the first "seq":<n> from each via grep + cut
    # so we don't need jq.
    head_resp=$(curl -fsS "${URL}/head?scope=${SCOPE}&limit=1")
    tail_resp=$(curl -fsS "${URL}/tail?scope=${SCOPE}")
    seq_lo=$(printf '%s' "$head_resp" | grep -oE '"seq":[0-9]+' | head -1 | cut -d: -f2)
    seq_hi=$(printf '%s' "$tail_resp" | grep -oE '"seq":[0-9]+' | head -1 | cut -d: -f2)

    if [ -z "${seq_lo:-}" ] || [ -z "${seq_hi:-}" ]; then
        echo "step 2: could not read seq range from ${URL} (scope=${SCOPE} empty?)" >&2
        exit 1
    fi

    echo "step 2: GET /get?scope=${SCOPE}&seq=<random in ${seq_lo}..${seq_hi}>"
    echo "  wrk -t${WRK_THREADS} -c${WRK_CONNECTIONS} -d${WRK_DURATION} --latency"
    echo

    LUA_PATH=$(mktemp -t wrk-get-seq.lua.XXXXXX)
    trap 'rm -f "$LUA_PATH"' EXIT

    # Heredoc expands ${SCOPE}, $seq_lo, $seq_hi at write time. Lua
    # itself sees concrete numbers and a literal scope string.
    cat > "$LUA_PATH" <<LUA
local thread_count = 0
function setup(thread)
    thread:set("tid", thread_count)
    thread_count = thread_count + 1
end
function init(args)
    -- Each thread seeds its RNG independently so threads do not march
    -- in lock-step through the same sequence of seqs.
    math.randomseed(os.time() + (tid or 0) * 1000)
    bad_status = {}
end
function request()
    local seq = math.random(${seq_lo}, ${seq_hi})
    return wrk.format("GET", "/get?scope=${SCOPE}&seq=" .. seq)
end
function response(status)
    if status ~= 200 then
        bad_status[status] = (bad_status[status] or 0) + 1
    end
end
function done(summary, latency, requests)
    for s, n in pairs(bad_status) do
        io.stderr:write("BADSTATUS " .. s .. " " .. n .. "\n")
    end
end
LUA

    wrk -t"$WRK_THREADS" -c"$WRK_CONNECTIONS" -d"$WRK_DURATION" --latency \
        --timeout 2s -s "$LUA_PATH" "$URL"
fi
