<?php
// bench.php — per-call latency + throughput for the cgo hot-path
// functions of the scopecache FrankenPHP extension. Single-thread,
// pre-warmed, fixed payload (default 54 bytes; configurable for the
// payload-sweep mode).
//
//   GET /bench.php?iter=100000&warmup=5000&tail_limit=10&payload=54
//
// Modes:
//   - Default ($payload = 54): bench all three hot-path functions
//     (scopecache_get, scopecache_tail, scopecache_append) and
//     report the per-call cost + the tail per-element overhead.
//   - Payload sweep (any $payload override): bench scopecache_get
//     only at the requested payload size; bench.sh --sweep drives
//     this in a loop across multiple sizes.

header('Content-Type: text/plain; charset=utf-8');
set_time_limit(0);

$ITER       = (int)($_GET['iter']       ?? 100000);
$WARMUP     = (int)($_GET['warmup']     ?? 5000);
$TAIL_LIMIT = (int)($_GET['tail_limit'] ?? 10);

// Single-op (sweep) mode is selected by the *presence* of ?payload=,
// not its value — so ?payload=54 still picks single-op, distinct
// from the no-arg call which runs the full 3-op bench.
$SINGLE_OP_MODE = isset($_GET['payload']);
$PAYLOAD        = $SINGLE_OP_MODE ? (int)$_GET['payload'] : 54;

function wipe_scope(string $scope): void {
    $ch = curl_init('http://127.0.0.1:8080/delete_scope');
    curl_setopt_array($ch, [
        CURLOPT_POST           => true,
        CURLOPT_HTTPHEADER     => ['Content-Type: application/json'],
        CURLOPT_POSTFIELDS     => json_encode(['scope' => $scope]),
        CURLOPT_RETURNTRANSFER => true,
    ]);
    curl_exec($ch);
    unset($ch);
}

// Build a JSON-string payload of approximately $PAYLOAD bytes. JSON
// quotes add 2 chars; pad with ($PAYLOAD - 2) filler bytes. Default
// no-arg mode (full 3-op bench) uses a recognisable greeting string
// so the wire content matches smoke/test.php; sweep mode uses filler.
if (!$SINGLE_OP_MODE) {
    $payload = json_encode(['greeting' => 'hi from scopecache, via cgo, in-process']);
} else {
    $filler_len = max($PAYLOAD - 2, 1);
    $payload    = json_encode(str_repeat('a', $filler_len));
}
$payload_bytes = strlen($payload);

$get_scope    = 'bench-get';
$tail_scope   = 'bench-tail';
$append_scope = 'bench-append';

wipe_scope($get_scope);
wipe_scope($tail_scope);
wipe_scope($append_scope);

scopecache_append($get_scope, "item-{$PAYLOAD}", $payload);
for ($i = 0; $i < $TAIL_LIMIT; $i++) {
    scopecache_append($tail_scope, "item-$i", $payload);
}

// Sanity probes.
$probe_get = scopecache_get($get_scope, "item-{$PAYLOAD}");
if (!is_array($probe_get) || ($probe_get['hit'] ?? null) !== true) {
    die("seed FAILED: get probe envelope " . var_export($probe_get, true) . "\n");
}
$probe_tail = scopecache_tail($tail_scope, $TAIL_LIMIT);
if (!is_array($probe_tail) || ($probe_tail['count'] ?? -1) !== $TAIL_LIMIT) {
    die("seed FAILED: tail count=" . ($probe_tail['count'] ?? 'null') . ", expected $TAIL_LIMIT\n");
}

printf("payload bytes : %d\n",  $payload_bytes);
printf("tail_limit    : %d\n",  $TAIL_LIMIT);
printf("iterations    : %d (warmup %d)\n", $ITER, $WARMUP);

// Payload-sweep mode (caller passed ?payload=...): bench scopecache_get
// only and emit a single-row report bench.sh's --sweep can parse.
if ($SINGLE_OP_MODE) {
    for ($i = 0; $i < $WARMUP; $i++) scopecache_get($get_scope, "item-{$PAYLOAD}");
    $t0 = hrtime(true);
    for ($i = 0; $i < $ITER; $i++)   { $r = scopecache_get($get_scope, "item-{$PAYLOAD}"); }
    $ns  = hrtime(true) - $t0;
    $per = $ns / $ITER;
    $qps = 1e9 * $ITER / $ns;
    echo "\n";
    printf("per call      : %.1f ns\n",    $per);
    printf("throughput    : %.0f calls/sec\n", $qps);
    return;
}

// Default mode: all three hot-path functions.
echo "\n";
printf("%-38s | %-12s | %-15s\n", "op", "per call", "throughput");
printf("%s\n", str_repeat('-', 75));

// scopecache_get.
for ($i = 0; $i < $WARMUP; $i++) scopecache_get($get_scope, "item-{$PAYLOAD}");
$t0     = hrtime(true);
for ($i = 0; $i < $ITER; $i++)   { $r = scopecache_get($get_scope, "item-{$PAYLOAD}"); }
$ns_get = hrtime(true) - $t0;

// scopecache_tail.
for ($i = 0; $i < $WARMUP; $i++) scopecache_tail($tail_scope, $TAIL_LIMIT);
$t0      = hrtime(true);
for ($i = 0; $i < $ITER; $i++)   { $r = scopecache_tail($tail_scope, $TAIL_LIMIT); }
$ns_tail = hrtime(true) - $t0;

// scopecache_append. Capped at 30k seq-only items so we stay under
// the default per-scope item budget; 30k × ~100 B = ~3 MB.
$APPEND_ITER   = min($ITER, 30000);
$APPEND_WARMUP = min($WARMUP, 500);
for ($i = 0; $i < $APPEND_WARMUP; $i++) scopecache_append($append_scope, '', $payload);
$t0        = hrtime(true);
for ($i = 0; $i < $APPEND_ITER; $i++)   { $s = scopecache_append($append_scope, '', $payload); }
$ns_append = hrtime(true) - $t0;

$row = function(string $label, float $ns, int $iter) {
    $per = $ns / $iter;
    $qps = 1e9 * $iter / $ns;
    printf("%-38s | %-12s | %-15s\n",
        $label,
        sprintf("%.1f ns", $per),
        number_format($qps, 0, '.', ' ') . ' /s');
};

$row("scopecache_get",                                $ns_get,    $ITER);
$row("scopecache_tail (limit=$TAIL_LIMIT)",           $ns_tail,   $ITER);
$row("scopecache_append (seq-only, n=$APPEND_ITER)",  $ns_append, $APPEND_ITER);

echo "\n";
printf("scopecache_tail per-element overhead : %.1f ns/elt\n",
    ($ns_tail / $ITER - $ns_get / $ITER) / $TAIL_LIMIT);
printf("scopecache_append vs scopecache_get  : %.1fx (relative cost)\n",
    ($ns_append / $APPEND_ITER) / ($ns_get / $ITER));
