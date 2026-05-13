<?php
// test.php — minimal demo of the in-process PHP→scopecache call,
// now returning HTTP-wire-shaped PHP arrays everywhere.
//
// What this proves:
//   - PHP can write to the cache via HTTP /append (goes through the
//     scopecache caddymodule's *Gateway).
//   - PHP can read the same item back via scopecache_get() (which
//     LookupGateway("default")s the SAME *Gateway — no second cache).
//   - A miss returns the same envelope shape as HTTP — hit=false,
//     item=null — not a PHP null.
//   - Writes return the HTTP /append envelope with created=true plus
//     the assigned seq under 'item.seq'.
//
// Must run inside a binary that has the scopecache caddymodule
// configured in the Caddyfile (so Provision() ran and registered
// the gateway). Plain `frankenphp php-cli test.php` will show "no
// gateway" results because no caddymodule is active — the extension's
// LookupGateway returns nil. To exercise the actual round-trip:
//
//   ./dist/frankenphp run --config Caddyfile.bench
//   curl http://localhost:8080/test.php

header('Content-Type: text/plain; charset=utf-8');

echo "=== scopecache PHP extension — registry-aware demo ===\n\n";

// Step 1: seed an item via HTTP /append. This goes through the
// scopecache caddymodule's *Gateway — the SAME gateway the extension
// will read from via LookupGateway("default").
$seed_body = json_encode([
    'scope'   => 'demo',
    'id'      => 'hello',
    'payload' => ['greeting' => 'hi from scopecache, via cgo, in-process'],
]);
$ch = curl_init('http://127.0.0.1:8080/append');
curl_setopt_array($ch, [
    CURLOPT_POST           => true,
    CURLOPT_HTTPHEADER     => ['Content-Type: application/json'],
    CURLOPT_POSTFIELDS     => $seed_body,
    CURLOPT_RETURNTRANSFER => true,
]);
curl_exec($ch);
$seed_status = curl_getinfo($ch, CURLINFO_HTTP_CODE);
unset($ch);
$seed_note = ($seed_status == 409) ? " (already existed, OK)" : "";
echo "Seed POST /append          -> HTTP $seed_status$seed_note\n\n";

// Step 2: hit — pre-seeded item, returns the /get envelope.
$got = scopecache_get('demo', 'hello');
echo "scopecache_get('demo', 'hello') -> ";
var_dump($got);

// Step 3: miss — unknown id within a known scope. hit=false envelope.
$miss = scopecache_get('demo', 'no-such-item');
echo "scopecache_get('demo', 'no-such-item') -> ";
var_dump($miss);

// Step 4: miss — unknown scope entirely. Same shape; cache treats
// unknown scope identically to unknown id.
$miss_scope = scopecache_get('no-such-scope', 'hello');
echo "scopecache_get('no-such-scope', 'hello') -> ";
var_dump($miss_scope);

echo "\n=== scopecache_append envelope ===\n\n";

// Append from PHP side: returns /append envelope with created=true
// and item.seq cache-assigned (>= 1). We use a fresh id each run so
// we don't collide with prior seeds.
$append_id = 'php-write-' . bin2hex(random_bytes(4));
$append_payload = json_encode(['written' => 'from PHP via scopecache_append']);
$append_env = scopecache_append('demo', $append_id, $append_payload);
echo "scopecache_append('demo', '$append_id', ...) -> ";
var_dump($append_env);

// Read back what we just wrote.
$readback = scopecache_get('demo', $append_id);
echo "scopecache_get('demo', '$append_id') -> ";
var_dump($readback);

// Append into a never-seen scope creates it implicitly (scopecache
// has no separate scope-create primitive).
$bootstrap_id = 'bootstrap-' . bin2hex(random_bytes(4));
$bootstrap_env = scopecache_append('php-side-scope', $bootstrap_id, '"hi"');
echo "scopecache_append('php-side-scope', '$bootstrap_id', '\"hi\"') -> ";
var_dump($bootstrap_env);

echo "\n=== scopecache_tail envelope ===\n\n";

// Hit: tail returns /tail envelope with items[]. The seeded 'demo'
// scope should have at least the seed plus the two appends above.
$tail_hit = scopecache_tail('demo', 5);
echo "scopecache_tail('demo', 5) -> ";
var_dump($tail_hit);

// Miss: tail on unknown scope returns the same shape with hit=false,
// items=[]. (Diverges from the pre-rewrite contract that returned
// PHP null on miss; matches HTTP /tail exactly.)
$tail_miss = scopecache_tail('no-such-scope', 5);
echo "scopecache_tail('no-such-scope', 5) -> ";
var_dump($tail_miss);

echo "\n";
echo "Every line above shows an associative array with an 'ok' key.\n";
echo "'hit'/'created' tell you the call outcome; 'item' or 'items'\n";
echo "carries the data. Payloads are already decoded — no json_decode\n";
echo "needed on the PHP side.\n";
echo "\n";
echo "If all lines show NULL: no caddymodule was loaded. Check that\n";
echo "your Caddyfile has a `scopecache { ... }` block and that the\n";
echo "binary is running via `frankenphp run --config <file>` (not\n";
echo "`frankenphp php-cli`, which skips Provision entirely).\n";
