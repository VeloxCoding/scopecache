<?php
// bench.php — micro-benchmark comparing five paths to a single
// 54-byte string value:
//
//   1. scopecache_get($scope, $id)             — cgo, in-process
//   2. curl -> HTTP -> scopecache module       — persistent handle
//   3. curl -> HTTP -> scopecache module       — fresh handle/call
//   4. phpredis  -> TCP -> redis               — persistent connection
//   5. phpredis  -> TCP -> redis               — fresh connection/call
//
// Paths 2,3 hit scopecache's Caddy module on 127.0.0.1:8080.
// Paths 4,5 hit Redis on redis-bench:6379 via the phpredis C-extension
// (extension_loaded('redis') = the standard production PHP client).
// All paths return the SAME 54-byte payload that the extension
// pre-seeds for demo/hello and that we seed below for bench/item.
//
// Tunable via query string: ?iterations=100000&warmup=5000

header('Content-Type: text/plain; charset=utf-8');
set_time_limit(0);  // bench can run >30s with the N-pattern section

$ITERATIONS = (int) ($_GET['iterations'] ?? 50000);
$WARMUP     = (int) ($_GET['warmup']     ?? 2000);

// Same scope/id for both extension and HTTP paths: with the shared
// *Gateway, they read the same memory, so any per-call delta is
// transport, not item-position-in-buffer.
$EXT_SCOPE  = 'bench';
$EXT_ID     = 'item';
$HTTP_URL   = 'http://127.0.0.1:8080/get?scope=bench&id=item';
$APPEND_URL = 'http://127.0.0.1:8080/append';

echo "=== scopecache extension (cgo) vs scopecache HTTP vs Redis ===\n\n";
echo "iterations: $ITERATIONS (after $WARMUP warmup)\n";
echo "PHP: " . PHP_VERSION . " (ZTS: " . (ZEND_THREAD_SAFE ? "yes" : "no") . ")\n";
echo "phpredis:   " . (extension_loaded('redis') ? phpversion('redis') : 'NOT LOADED') . "\n";
echo "memcached:  " . (extension_loaded('memcached') ? phpversion('memcached') : 'NOT LOADED') . "\n\n";

$REDIS_HOST = getenv('REDIS_HOST') ?: 'redis-bench';
$REDIS_PORT = (int)(getenv('REDIS_PORT') ?: 6379);
$REDIS_KEY  = 'bench:item';
$MC_HOST    = getenv('MC_HOST') ?: 'memcached-bench';
$MC_PORT    = (int)(getenv('MC_PORT') ?: 11211);
$MC_KEY     = 'bench:item';
$MC_VALUE   = '{"greeting":"hi from scopecache, via cgo, in-process"}';

// ---- Seed both demo/hello and bench/item on the shared Gateway --
// Since the registry switch, PHP and HTTP share ONE *Gateway. We
// seed both items via /append so the extension path (demo/hello)
// and the HTTP path (bench/item) measure the same shape of payload
// over the same memory.

$seed_payload = ['greeting' => 'hi from scopecache, via cgo, in-process'];
$append_body = json_encode([
    'scope'   => 'bench',
    'id'      => 'item',
    'payload' => $seed_payload,
]);
$ch = curl_init($APPEND_URL);
curl_setopt_array($ch, [
    CURLOPT_POST            => true,
    CURLOPT_HTTPHEADER      => ['Content-Type: application/json'],
    CURLOPT_POSTFIELDS      => $append_body,
    CURLOPT_RETURNTRANSFER  => true,
]);
$resp        = curl_exec($ch);
$seed_status = curl_getinfo($ch, CURLINFO_HTTP_CODE);
unset($ch);
$seed_note = ($seed_status == 409) ? " (already exists, OK)" : "";
echo "seed POST /append          -> HTTP $seed_status$seed_note\n";

$ext_body = scopecache_get($EXT_SCOPE, $EXT_ID);
echo "scopecache_get('$EXT_SCOPE','$EXT_ID')  -> " . strlen((string)$ext_body) . " bytes (raw payload)\n";

$ch = curl_init($HTTP_URL);
curl_setopt($ch, CURLOPT_RETURNTRANSFER, true);
$http_body = curl_exec($ch);
unset($ch);
echo "GET /get?scope=bench&id=item -> " . strlen((string)$http_body) . " bytes (envelope incl. payload)\n";
echo "\n";

// ---- Warmup all three paths --------------------------------------

for ($i = 0; $i < $WARMUP; $i++) {
    scopecache_get($EXT_SCOPE, $EXT_ID);
}
$ch = curl_init($HTTP_URL);
curl_setopt($ch, CURLOPT_RETURNTRANSFER, true);
for ($i = 0; $i < $WARMUP; $i++) {
    curl_exec($ch);
}
unset($ch);
// fresh-handle path: scale warmup down — it is much slower
for ($i = 0; $i < min($WARMUP, 500); $i++) {
    $ch = curl_init($HTTP_URL);
    curl_setopt($ch, CURLOPT_RETURNTRANSFER, true);
    curl_exec($ch);
    unset($ch);
}

// ---- Bench 1: extension path ------------------------------------

$start = hrtime(true);
for ($i = 0; $i < $ITERATIONS; $i++) {
    $r = scopecache_get($EXT_SCOPE, $EXT_ID);
}
$ext_ns     = hrtime(true) - $start;
$ext_per_op = $ext_ns / $ITERATIONS;
$ext_qps    = 1e9 * $ITERATIONS / $ext_ns;

// ---- Bench 2: HTTP path, persistent curl handle -----------------

$ch = curl_init($HTTP_URL);
curl_setopt($ch, CURLOPT_RETURNTRANSFER, true);
$start = hrtime(true);
for ($i = 0; $i < $ITERATIONS; $i++) {
    $r = curl_exec($ch);
}
$http_keep_ns = hrtime(true) - $start;
unset($ch);
$http_keep_per_op = $http_keep_ns / $ITERATIONS;
$http_keep_qps    = 1e9 * $ITERATIONS / $http_keep_ns;

// ---- Bench 3: HTTP path, fresh curl handle per call -------------
// Cap the fresh-handle bench at 5000 iterations so it finishes in
// reasonable wall time. Per-op accuracy at 5k is fine — each call
// burns ~1 ms, far above the hrtime resolution floor.
$HTTP_FRESH_ITER = min($ITERATIONS, 5000);
$start = hrtime(true);
for ($i = 0; $i < $HTTP_FRESH_ITER; $i++) {
    $ch = curl_init($HTTP_URL);
    curl_setopt($ch, CURLOPT_RETURNTRANSFER, true);
    $r = curl_exec($ch);
    unset($ch);
}
$http_fresh_ns = hrtime(true) - $start;
$http_fresh_per_op = $http_fresh_ns / $HTTP_FRESH_ITER;
$http_fresh_qps    = 1e9 * $HTTP_FRESH_ITER / $http_fresh_ns;

// ---- Bench 4: Redis path, persistent connection -----------------

$redis = new Redis();
$redis->connect($REDIS_HOST, $REDIS_PORT);
$probe = $redis->get($REDIS_KEY);
if ($probe === false) {
    // Should not happen — entrypoint seeds bench:item. Re-seed defensively.
    $redis->set($REDIS_KEY, '{"greeting":"hi from scopecache, via cgo, in-process"}');
    $probe = $redis->get($REDIS_KEY);
}
echo "Redis  GET $REDIS_KEY            -> " . strlen((string)$probe) . " bytes (raw value)\n\n";

// warmup
for ($i = 0; $i < $WARMUP; $i++) { $redis->get($REDIS_KEY); }

$start = hrtime(true);
for ($i = 0; $i < $ITERATIONS; $i++) {
    $r = $redis->get($REDIS_KEY);
}
$redis_keep_ns = hrtime(true) - $start;
$redis_keep_per_op = $redis_keep_ns / $ITERATIONS;
$redis_keep_qps    = 1e9 * $ITERATIONS / $redis_keep_ns;
$redis->close();

// ---- Bench 5: Redis path, fresh connection per call -------------
// Cap at the same iterations as the fresh-handle HTTP bench so the
// two "no connection pool" results sit on the same axis.
$REDIS_FRESH_ITER = min($ITERATIONS, 5000);

// short warmup for fresh-connect
for ($i = 0; $i < 100; $i++) {
    $r = new Redis();
    $r->connect($REDIS_HOST, $REDIS_PORT);
    $r->get($REDIS_KEY);
    $r->close();
}

$start = hrtime(true);
for ($i = 0; $i < $REDIS_FRESH_ITER; $i++) {
    $r = new Redis();
    $r->connect($REDIS_HOST, $REDIS_PORT);
    $v = $r->get($REDIS_KEY);
    $r->close();
}
$redis_fresh_ns = hrtime(true) - $start;
$redis_fresh_per_op = $redis_fresh_ns / $REDIS_FRESH_ITER;
$redis_fresh_qps    = 1e9 * $REDIS_FRESH_ITER / $redis_fresh_ns;

// ---- Bench 6: Memcached path, persistent connection ------------

$mc = new Memcached();
$mc->addServer($MC_HOST, $MC_PORT);
$mc->set($MC_KEY, $MC_VALUE);  // idempotent seed
$mc_probe = $mc->get($MC_KEY);
echo "Memcached GET $MC_KEY      -> " . strlen((string)$mc_probe) . " bytes (raw value)\n\n";

// warmup
for ($i = 0; $i < $WARMUP; $i++) { $mc->get($MC_KEY); }

$start = hrtime(true);
for ($i = 0; $i < $ITERATIONS; $i++) {
    $r = $mc->get($MC_KEY);
}
$mc_keep_ns = hrtime(true) - $start;
$mc_keep_per_op = $mc_keep_ns / $ITERATIONS;
$mc_keep_qps    = 1e9 * $ITERATIONS / $mc_keep_ns;
$mc->quit();

// ---- Bench 7: Memcached path, fresh connection per call --------
$MC_FRESH_ITER = min($ITERATIONS, 5000);

// short warmup
for ($i = 0; $i < 100; $i++) {
    $m = new Memcached();
    $m->addServer($MC_HOST, $MC_PORT);
    $m->get($MC_KEY);
    $m->quit();
}

$start = hrtime(true);
for ($i = 0; $i < $MC_FRESH_ITER; $i++) {
    $m = new Memcached();
    $m->addServer($MC_HOST, $MC_PORT);
    $v = $m->get($MC_KEY);
    $m->quit();
}
$mc_fresh_ns = hrtime(true) - $start;
$mc_fresh_per_op = $mc_fresh_ns / $MC_FRESH_ITER;
$mc_fresh_qps    = 1e9 * $MC_FRESH_ITER / $mc_fresh_ns;

// ---- Bench 8: realistic PHP-script pattern --------------------
// "1 connect + N GETs + 1 close", repeated M times. Measures what
// a real PHP script that does N Redis lookups per request actually
// pays, INCLUDING the connect handshake spread over those N calls.
//
// We also run the same N-count pattern through the extension for
// direct comparison — but for the extension there is no connect/
// close, so the cost is simply N × per-call.

$N_VARIANTS = [1, 4, 100, 1000];

$redis_per_script = [];
$mc_per_script    = [];
$ext_per_script   = [];

foreach ($N_VARIANTS as $N) {
    $iter = match (true) {
        $N <= 4     => 1000,
        $N <= 100   => 200,
        default     => 30,
    };

    // Redis: open conn, do N GETs, close conn — repeat $iter times
    $start = hrtime(true);
    for ($i = 0; $i < $iter; $i++) {
        $r = new Redis();
        $r->connect($REDIS_HOST, $REDIS_PORT);
        for ($j = 0; $j < $N; $j++) {
            $r->get($REDIS_KEY);
        }
        $r->close();
    }
    $redis_per_script[$N] = (hrtime(true) - $start) / $iter;

    // Memcached: same pattern
    $start = hrtime(true);
    for ($i = 0; $i < $iter; $i++) {
        $m = new Memcached();
        $m->addServer($MC_HOST, $MC_PORT);
        for ($j = 0; $j < $N; $j++) {
            $m->get($MC_KEY);
        }
        $m->quit();
    }
    $mc_per_script[$N] = (hrtime(true) - $start) / $iter;

    // Extension: simply N calls.
    $ext_iter = max($iter * 10, 1000);
    $start = hrtime(true);
    for ($i = 0; $i < $ext_iter; $i++) {
        for ($j = 0; $j < $N; $j++) {
            scopecache_get($EXT_SCOPE, $EXT_ID);
        }
    }
    $ext_per_script[$N] = (hrtime(true) - $start) / $ext_iter;
}

// ---- Report -----------------------------------------------------

$fmt_int = function (float $v): string {
    return number_format($v, 0, '.', ' ');
};
$fmt_us  = function (float $ns): string {
    if ($ns < 1000) return sprintf("%6.0f ns", $ns);
    if ($ns < 1_000_000) return sprintf("%6.2f us", $ns / 1000);
    return sprintf("%6.2f ms", $ns / 1_000_000);
};

echo "Path                                              | per call   | req/sec    |   iter | total\n";
echo "--------------------------------------------------|------------|------------|--------|----------\n";
printf("1) scopecache_get()       (cgo, in-process)        | %10s | %10s | %6d | %.1f ms\n",
    $fmt_us($ext_per_op),       $fmt_int($ext_qps),       $ITERATIONS,        $ext_ns / 1e6);
printf("2) scopecache HTTP        (curl, persistent)       | %10s | %10s | %6d | %.1f ms\n",
    $fmt_us($http_keep_per_op),  $fmt_int($http_keep_qps),  $ITERATIONS,       $http_keep_ns / 1e6);
printf("3) scopecache HTTP        (curl, fresh per call)   | %10s | %10s | %6d | %.1f ms\n",
    $fmt_us($http_fresh_per_op), $fmt_int($http_fresh_qps), $HTTP_FRESH_ITER,  $http_fresh_ns / 1e6);
printf("4) Redis GET              (phpredis, persistent)   | %10s | %10s | %6d | %.1f ms\n",
    $fmt_us($redis_keep_per_op), $fmt_int($redis_keep_qps), $ITERATIONS,       $redis_keep_ns / 1e6);
printf("5) Redis GET              (phpredis, fresh conn)   | %10s | %10s | %6d | %.1f ms\n",
    $fmt_us($redis_fresh_per_op),$fmt_int($redis_fresh_qps),$REDIS_FRESH_ITER, $redis_fresh_ns / 1e6);
printf("6) Memcached GET          (memcached, persistent)  | %10s | %10s | %6d | %.1f ms\n",
    $fmt_us($mc_keep_per_op),    $fmt_int($mc_keep_qps),    $ITERATIONS,       $mc_keep_ns / 1e6);
printf("7) Memcached GET          (memcached, fresh conn)  | %10s | %10s | %6d | %.1f ms\n",
    $fmt_us($mc_fresh_per_op),   $fmt_int($mc_fresh_qps),   $MC_FRESH_ITER,    $mc_fresh_ns / 1e6);
echo "\n";
printf("Speedup ext vs scopecache HTTP persistent  : %6.0fx\n",  $http_keep_per_op   / $ext_per_op);
printf("Speedup ext vs scopecache HTTP fresh       : %6.0fx\n",  $http_fresh_per_op  / $ext_per_op);
printf("Speedup ext vs Redis     (phpredis,  persistent) : %6.0fx\n", $redis_keep_per_op  / $ext_per_op);
printf("Speedup ext vs Redis     (phpredis,  fresh conn) : %6.0fx\n", $redis_fresh_per_op / $ext_per_op);
printf("Speedup ext vs Memcached (memcached, persistent) : %6.0fx\n", $mc_keep_per_op     / $ext_per_op);
printf("Speedup ext vs Memcached (memcached, fresh conn) : %6.0fx\n", $mc_fresh_per_op    / $ext_per_op);
echo "\n";
printf("Redis persistent vs Memcached persistent: %.2fx (%s faster)\n",
    max($redis_keep_per_op, $mc_keep_per_op) / min($redis_keep_per_op, $mc_keep_per_op),
    ($redis_keep_per_op < $mc_keep_per_op) ? "Redis" : "Memcached");

echo "\n";
echo "=== N-lookups-per-script pattern (realistic PHP shape) ===\n\n";
echo "Per script: open Redis connection, do N GETs, close. Average over many script-runs.\n";
echo "Extension column: just N calls, no setup. For comparison.\n\n";
echo "    N |  Redis total/script | Redis per call |  Mc total/script |  Mc per call | Ext total/script | Ext per call | vs Redis | vs Mc\n";
echo "------|---------------------|----------------|------------------|--------------|------------------|--------------|----------|--------\n";
foreach ($N_VARIANTS as $N) {
    $rps  = $redis_per_script[$N];
    $mps  = $mc_per_script[$N];
    $eps  = $ext_per_script[$N];
    printf(" %4d | %19s | %14s | %16s | %12s | %16s | %12s | %7.0fx | %5.0fx\n",
        $N,
        $fmt_us($rps),
        $fmt_us($rps / $N),
        $fmt_us($mps),
        $fmt_us($mps / $N),
        $fmt_us($eps),
        $fmt_us($eps / $N),
        $rps / $eps,
        $mps / $eps);
}
echo "\n";
echo "How the Redis number scales: total ~= connect-cost + N * per-call. As N grows,\n";
echo "the per-call number drops toward the steady-state ~127 us (path 4 in the\n";
echo "first table); for small N the connect handshake dominates.\n";
echo "\n";
echo "Extension scales as: total = N * ~0.3 us. No connect cost, ever.\n";
