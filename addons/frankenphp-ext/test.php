<?php
// test.php — minimal demo of the in-process PHP→scopecache call,
// now registry-aware.
//
// What this proves:
//   - PHP can write to the cache via HTTP /append (goes through the
//     scopecache caddymodule's *Gateway).
//   - PHP can read the same item back via scopecache_get() (which
//     LookupGateway("default")s the SAME *Gateway — no second cache).
//   - A miss on a known scope returns NULL.
//   - A miss on an unknown scope returns NULL.
//
// Must run inside a binary that has the scopecache caddymodule
// configured in the Caddyfile (so Provision() ran and registered
// the gateway). Plain `frankenphp php-cli test.php` will show three
// NULLs because no caddymodule is active — the extension's
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

// Step 2: hit — pre-seeded item, should return the JSON payload.
$payload = scopecache_get('demo', 'hello');
echo "scopecache_get('demo', 'hello') -> ";
var_dump($payload);

// Step 3: miss — unknown id within a known scope.
$miss = scopecache_get('demo', 'no-such-item');
echo "scopecache_get('demo', 'no-such-item') -> ";
var_dump($miss);

// Step 4: miss — unknown scope entirely.
$miss_scope = scopecache_get('no-such-scope', 'hello');
echo "scopecache_get('no-such-scope', 'hello') -> ";
var_dump($miss_scope);

echo "\n";
echo "If the first call returned a JSON-shaped string and the other two\n";
echo "returned NULL, the extension is correctly sharing state with HTTP\n";
echo "clients through the gateway registry.\n";

echo "\n";
echo "If ALL THREE returned NULL: no caddymodule was loaded. Check that\n";
echo "your Caddyfile has a `scopecache { ... }` block and that the\n";
echo "binary is running via `frankenphp run --config <file>` (not\n";
echo "`frankenphp php-cli`, which skips Provision entirely).\n";
