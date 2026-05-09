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
#   WRK_THREADS=1            wrk worker threads in step 2
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
WRK_THREADS="${WRK_THREADS:-1}"
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
    # the oldest items, /tail the newest — but /tail's default window
    # is 1000 items, not 1, so we scan ALL "seq":<n> matches in each
    # response and pick the extreme. Done in a single awk pass to keep
    # set -o pipefail happy (a `grep | head -1` pipeline can SIGPIPE
    # grep on the second match and silently abort the script).
    seq_extreme() {
        awk -v mode="$1" '
            {
                src = $0
                while (match(src, /"seq":[0-9]+/)) {
                    v = substr(src, RSTART + 6, RLENGTH - 6) + 0
                    if (count++ == 0) { best = v }
                    else if (mode == "max" && v > best) { best = v }
                    else if (mode == "min" && v < best) { best = v }
                    src = substr(src, RSTART + RLENGTH)
                }
            }
            END { if (count > 0) print best }
        '
    }

    head_resp=$(curl -fsS "${URL}/head?scope=${SCOPE}&limit=1")
    tail_resp=$(curl -fsS "${URL}/tail?scope=${SCOPE}")
    seq_lo=$(printf '%s' "$head_resp" | seq_extreme min)
    seq_hi=$(printf '%s' "$tail_resp" | seq_extreme max)

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
-- bad_status lives at top level so it exists in BOTH the worker-
-- thread Lua state (where response() writes) AND the master state
-- (where done() reads). Defining it inside init() leaks it to
-- workers only and panics done() with "table expected, got nil".
bad_status = {}
local thread_count = 0
function setup(thread)
    thread:set("tid", thread_count)
    thread_count = thread_count + 1
end
function init(args)
    -- Each thread seeds its RNG independently so threads do not march
    -- in lock-step through the same sequence of seqs.
    math.randomseed(os.time() + (tid or 0) * 1000)
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
    -- wrk's --latency only prints 50/75/90/99 by default; emit p95
    -- ourselves on stdout with a distinct prefix so the bash wrapper
    -- can pull it out and reformat the summary.
    io.write(string.format("P95_US %d\n", latency:percentile(95)))
    for s, n in pairs(bad_status) do
        io.stderr:write("BADSTATUS " .. s .. " " .. n .. "\n")
    end
end
LUA

    # Capture wrk output and reformat into a condensed summary. The
    # full wrk output (Thread Stats with stdev, Latency Distribution
    # block, Transfer/sec, etc.) is suppressed; we keep only:
    #   - per-thread Avg req/s (the "93.00k" style number)
    #   - latency avg / p50 / p95 / max
    #   - the "N requests in Ts, NMB read" totals line
    wrk_out=$(wrk -t"$WRK_THREADS" -c"$WRK_CONNECTIONS" -d"$WRK_DURATION" \
        --latency --timeout 2s -s "$LUA_PATH" "$URL" 2>&1)

    # Parse fields. Each awk line bails on first match (exit) so this
    # is one stream-pass per field; cheap.
    lat_avg=$(printf '%s\n' "$wrk_out" | awk '/^[[:space:]]+Latency[[:space:]]/ {print $2; exit}')
    lat_max=$(printf '%s\n' "$wrk_out" | awk '/^[[:space:]]+Latency[[:space:]]/ {print $4; exit}')
    rps_avg=$(printf '%s\n' "$wrk_out" | awk '/^[[:space:]]+Req\/Sec[[:space:]]/ {print $2; exit}')
    p50=$(printf '%s\n'    "$wrk_out" | awk '/^[[:space:]]+50%[[:space:]]/ {print $2; exit}')
    p95_us=$(printf '%s\n' "$wrk_out" | awk '/^P95_US[[:space:]]/ {print $2; exit}')
    totals=$(printf '%s\n' "$wrk_out" | awk '/^[[:space:]]+[0-9]+[[:space:]]requests in/ {sub(/^[[:space:]]+/, ""); print; exit}')

    # Format p95 (in microseconds from Lua) the way wrk formats latencies.
    fmt_us() {
        awk -v v="${1:-0}" 'BEGIN {
            if (v >= 1000000) printf "%.2fs", v/1000000
            else if (v >= 1000) printf "%.2fms", v/1000
            else printf "%dus", v
        }'
    }
    p95=$(fmt_us "${p95_us:-0}")

    printf '  Req/sec:  %s\n'                        "${rps_avg:-?}"
    printf '  Latency:  avg=%s  p50=%s  p95=%s  max=%s\n' \
        "${lat_avg:-?}" "${p50:-?}" "$p95" "${lat_max:-?}"
    printf '  Total:    %s\n'                        "${totals:-?}"

    # If wrk wrote anything to stderr (BADSTATUS lines, errors), pass
    # it through so the operator notices.
    bad_lines=$(printf '%s\n' "$wrk_out" | grep -E '^(BADSTATUS|wrk:|Socket errors:|Non-2xx)' || true)
    if [ -n "$bad_lines" ]; then
        printf '\n'
        printf '%s\n' "$bad_lines"
    fi
fi
