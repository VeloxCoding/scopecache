<?php
// bench-ext-only.php — measures ONLY the cgo path (scopecache_get).
// Used to compare two extension builds against each other, or to
// trace per-call cost across payload sizes. No Redis/Memcached deps —
// runs in any FrankenPHP+scopecache binary.
//
// Query params:
//   payload  size of the JSON payload in bytes (default 54)
//   iter     measured iterations (default 200000)
//   warmup   warmup iterations (default 5000)
//
// Examples:
//   /bench-ext-only.php
//   /bench-ext-only.php?payload=1024&iter=100000
//   /bench-ext-only.php?payload=10240&iter=20000

header('Content-Type: text/plain; charset=utf-8');
set_time_limit(0);

$ITER    = (int)($_GET['iter']    ?? 200000);
$WARMUP  = (int)($_GET['warmup']  ?? 5000);
$PAYLOAD = (int)($_GET['payload'] ?? 54);

// Build a JSON-string payload of approximately $PAYLOAD bytes on the
// wire. JSON encoding of a PHP string adds two quote chars, so we
// pad with ($PAYLOAD - 2) filler chars. Each item gets its own id so
// repeated /append for different sizes don't collide.
$filler_len = max($PAYLOAD - 2, 1);
$payload    = str_repeat('a', $filler_len);
$id         = "item-{$PAYLOAD}";

$seed = json_encode([
    'scope'   => 'bench',
    'id'      => $id,
    'payload' => $payload,
]);

$ch = curl_init('http://127.0.0.1:8080/append');
curl_setopt_array($ch, [
    CURLOPT_POST           => true,
    CURLOPT_HTTPHEADER     => ['Content-Type: application/json'],
    CURLOPT_POSTFIELDS     => $seed,
    CURLOPT_RETURNTRANSFER => true,
]);
curl_exec($ch);
$seed_status = curl_getinfo($ch, CURLINFO_HTTP_CODE);
unset($ch);

if ($seed_status !== 200 && $seed_status !== 409) {
    die("seed failed: HTTP $seed_status\n");
}

// Sanity.
$probe = scopecache_get('bench', $id);
if ($probe === null) {
    die("setup error: bench/{$id} not seeded\n");
}
$actual_bytes = strlen($probe);

// Warmup.
for ($i = 0; $i < $WARMUP; $i++) { scopecache_get('bench', $id); }

// Measure.
$t0 = hrtime(true);
for ($i = 0; $i < $ITER; $i++) { $r = scopecache_get('bench', $id); }
$ns = hrtime(true) - $t0;

$per_op_ns = $ns / $ITER;
$qps       = 1e9 * $ITER / $ns;

printf("requested     : %d bytes\n",  $PAYLOAD);
printf("actual        : %d bytes\n",  $actual_bytes);
printf("iterations    : %d (after %d warmup)\n", $ITER, $WARMUP);
printf("total         : %.1f ms\n",   $ns / 1e6);
printf("per call      : %.1f ns\n",   $per_op_ns);
printf("throughput    : %.0f calls/sec\n", $qps);
