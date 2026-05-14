<?php
// bench-experimental.php — A/B benchmarks for the scopecache_x_*
// experimental cgo entry-points defined in
// addons/frankenphp-ext/scopecache_ext_experimental.go.
//
// Compared to bench.php (the production-shipping ops), this file
// exists to investigate alternative implementations: pure cgo cost,
// payload-only returns, full-envelope json.Marshal cost, etc.
//
// Both files share the same harness style so reading one teaches
// the other. Subtract rows to isolate individual costs.

header('Content-Type: text/plain; charset=utf-8');
set_time_limit(0);

$ITER    = (int)($_GET['iter']    ?? 100000);
$WARMUP  = (int)($_GET['warmup']  ?? 5000);
$PAYLOAD = 54;

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

$payload    = json_encode(['greeting' => 'hi from scopecache, via cgo, in-process']);
$get_scope  = 'bench-x-get';

wipe_scope($get_scope);
scopecache_append($get_scope, "item-{$PAYLOAD}", $payload);

// Sanity probes.
$p1 = scopecache_get($get_scope, "item-{$PAYLOAD}");
if (!is_array($p1) || ($p1['hit'] ?? null) !== true) {
    die("seed FAILED: scopecache_get probe — " . var_export($p1, true) . "\n");
}
$p2 = scopecache_x_constant_json();
if (!is_string($p2) || strpos($p2, '"ok":true') === false) {
    die("seed FAILED: scopecache_x_constant_json probe — " . var_export($p2, true) . "\n");
}
$p3 = scopecache_x_payload_only($get_scope, "item-{$PAYLOAD}");
if (!is_string($p3) || $p3 !== $payload) {
    die("seed FAILED: scopecache_x_payload_only — " . var_export($p3, true) . "\n");
}
$p4 = scopecache_x_get_json($get_scope, "item-{$PAYLOAD}");
if (!is_string($p4) || strpos($p4, '"hit":true') === false) {
    die("seed FAILED: scopecache_x_get_json — " . var_export($p4, true) . "\n");
}
$p5 = scopecache_x_marshal_only();
if (!is_string($p5) || strpos($p5, '"hit":true') === false) {
    die("seed FAILED: scopecache_x_marshal_only — " . var_export($p5, true) . "\n");
}

printf("iterations    : %d (warmup %d)\n", $ITER, $WARMUP);
printf("payload bytes : %d\n", $PAYLOAD);
echo "\n";
printf("%-48s | %-12s | %-15s\n", "op", "per call", "throughput");
printf("%s\n", str_repeat('-', 85));

// ---- baseline: production scopecache_get (PHP-array return) -------
for ($i = 0; $i < $WARMUP; $i++) scopecache_get($get_scope, "item-{$PAYLOAD}");
$t0 = hrtime(true);
for ($i = 0; $i < $ITER; $i++)  { $r = scopecache_get($get_scope, "item-{$PAYLOAD}"); }
$ns_get = hrtime(true) - $t0;

// ---- pure cgo cost (no gateway, no marshal) -----------------------
for ($i = 0; $i < $WARMUP; $i++) scopecache_x_constant_json();
$t0 = hrtime(true);
for ($i = 0; $i < $ITER; $i++)  { $r = scopecache_x_constant_json(); }
$ns_const = hrtime(true) - $t0;

// ---- gateway + payload-only (no marshal, no envelope) -------------
for ($i = 0; $i < $WARMUP; $i++) scopecache_x_payload_only($get_scope, "item-{$PAYLOAD}");
$t0 = hrtime(true);
for ($i = 0; $i < $ITER; $i++)  { $r = scopecache_x_payload_only($get_scope, "item-{$PAYLOAD}"); }
$ns_payload = hrtime(true) - $t0;

// ---- gateway + full envelope via json.Marshal ---------------------
for ($i = 0; $i < $WARMUP; $i++) scopecache_x_get_json($get_scope, "item-{$PAYLOAD}");
$t0 = hrtime(true);
for ($i = 0; $i < $ITER; $i++)  { $r = scopecache_x_get_json($get_scope, "item-{$PAYLOAD}"); }
$ns_json_raw = hrtime(true) - $t0;

// ---- gateway + full envelope + PHP json_decode (real app cost) ----
$t0 = hrtime(true);
for ($i = 0; $i < $ITER; $i++)  { $r = json_decode(scopecache_x_get_json($get_scope, "item-{$PAYLOAD}"), true); }
$ns_json_decoded = hrtime(true) - $t0;

// ---- json.Marshal only (no gateway, no cgo-to-PHP-string) ---------
// scopecache_x_marshal_only still does the bytes->zend_string cgo
// crossing at the end, but skips the gateway lookup and uses a
// pre-built Item literal in Go. (constant) + Marshal cost lives here.
for ($i = 0; $i < $WARMUP; $i++) scopecache_x_marshal_only();
$t0 = hrtime(true);
for ($i = 0; $i < $ITER; $i++)  { $r = scopecache_x_marshal_only(); }
$ns_marshal = hrtime(true) - $t0;

// ---- Item.MarshalJSON in isolation --------------------------------
for ($i = 0; $i < $WARMUP; $i++) scopecache_x_marshal_item_only();
$t0 = hrtime(true);
for ($i = 0; $i < $ITER; $i++)  { $r = scopecache_x_marshal_item_only(); }
$ns_marshal_item = hrtime(true) - $t0;

// ---- envelope without Item (Item==nil) ----------------------------
for ($i = 0; $i < $WARMUP; $i++) scopecache_x_marshal_no_item();
$t0 = hrtime(true);
for ($i = 0; $i < $ITER; $i++)  { $r = scopecache_x_marshal_no_item(); }
$ns_marshal_no_item = hrtime(true) - $t0;

// ---- hand-rolled JSON envelope (alternative to json.Marshal) -------
for ($i = 0; $i < $WARMUP; $i++) scopecache_x_get_json_handrolled($get_scope, "item-{$PAYLOAD}");
$t0 = hrtime(true);
for ($i = 0; $i < $ITER; $i++)  { $r = scopecache_x_get_json_handrolled($get_scope, "item-{$PAYLOAD}"); }
$ns_handrolled_raw = hrtime(true) - $t0;

$t0 = hrtime(true);
for ($i = 0; $i < $ITER; $i++)  { $r = json_decode(scopecache_x_get_json_handrolled($get_scope, "item-{$PAYLOAD}"), true); }
$ns_handrolled_decoded = hrtime(true) - $t0;

$row = function(string $label, float $ns, int $iter) {
    $per = $ns / $iter;
    $qps = 1e9 * $iter / $ns;
    printf("%-48s | %-12s | %-15s\n",
        $label,
        sprintf("%.1f ns", $per),
        number_format($qps, 0, '.', ' ') . ' /s');
};

$row("scopecache_get (PHP-array; baseline)",            $ns_get,                $ITER);
$row("scopecache_x_constant_json (pure cgo)",           $ns_const,              $ITER);
$row("scopecache_x_payload_only (lookup+payload)",      $ns_payload,            $ITER);
$row("scopecache_x_marshal_only (Marshal full env)",    $ns_marshal,            $ITER);
$row("scopecache_x_marshal_item_only (Item alone)",     $ns_marshal_item,       $ITER);
$row("scopecache_x_marshal_no_item (env, Item=nil)",    $ns_marshal_no_item,    $ITER);
$row("scopecache_x_get_json (json.Marshal env)",        $ns_json_raw,           $ITER);
$row("scopecache_x_get_json + PHP json_decode",         $ns_json_decoded,       $ITER);
$row("scopecache_x_get_json_handrolled (raw)",          $ns_handrolled_raw,     $ITER);
$row("scopecache_x_get_json_handrolled + json_decode",  $ns_handrolled_decoded, $ITER);

echo "\n";
echo "Cost decomposition (subtractions):\n\n";

printf("  pure cgo (string-out + boundary)           : %.1f ns  [constant_json]\n",
    $ns_const / $ITER);
printf("  gateway lookup + payload copy              : %.1f ns  [payload_only - constant_json]\n",
    ($ns_payload - $ns_const) / $ITER);
printf("  Item.MarshalJSON in isolation              : %.1f ns  [marshal_item - constant_json]\n",
    ($ns_marshal_item - $ns_const) / $ITER);
printf("  envelope-wrap (Item=nil) marshal           : %.1f ns  [marshal_no_item - constant_json]\n",
    ($ns_marshal_no_item - $ns_const) / $ITER);
printf("  full envelope marshal (Item incl)          : %.1f ns  [marshal_only - constant_json]\n",
    ($ns_marshal - $ns_const) / $ITER);
printf("  hand-rolled JSON builder vs json.Marshal   : %.1fx faster  [marshal - constant] / [handrolled - lookup - constant]\n",
    (($ns_marshal - $ns_const) / $ITER) / max(1.0, ($ns_handrolled_raw - $ns_payload) / $ITER));
printf("  PHP json_decode of envelope                : %.1f ns  [decoded - raw]\n",
    ($ns_json_decoded - $ns_json_raw) / $ITER);
echo "\n";
echo "Three paths to the SAME PHP-array result:\n\n";
printf("  array build via cgo HashTable (current)     : %.1f ns\n", $ns_get / $ITER);
printf("  json.Marshal -> PHP json_decode             : %.1f ns\n", $ns_json_decoded / $ITER);
printf("  hand-rolled JSON build -> PHP json_decode   : %.1f ns\n", $ns_handrolled_decoded / $ITER);
