<?php
// validate-sprint1.php — beyond-smoke correctness checks for the
// scopecache_tail + scopecache_append surface. Adapted to the
// envelope-returning shape: each function returns a PHP array
// mirroring the HTTP JSON response, never PHP null/0 except when
// the extension cannot reach a *Gateway.
//
// Output is PASS/FAIL per check; validate-sprint1.sh greps it.

header('Content-Type: text/plain; charset=utf-8');
set_time_limit(0);

$pass = 0;
$fail = 0;

function check(string $label, bool $ok, string $detail = ''): void {
    global $pass, $fail;
    if ($ok) {
        echo "PASS  $label\n";
        $pass++;
    } else {
        echo "FAIL  $label" . ($detail !== '' ? "  ($detail)" : '') . "\n";
        $fail++;
    }
}

// Helpers — same as validate-sprint2.php so the two harnesses stay
// in lockstep.
function seq(array $env): int          { return $env['item']['seq'] ?? -1; }
function payloadOf(array $env): mixed  { return $env['item']['payload'] ?? null; }
function itemsOf(array $env): array    { return $env['items'] ?? []; }
function isErr(array $env): bool       { return ($env['ok'] ?? null) === false; }

echo "=== validate-sprint1.php ===\n\n";

// --- 0. Sanity: functions are loaded -----------------------------------------
check("function_exists scopecache_get",    function_exists('scopecache_get'));
check("function_exists scopecache_tail",   function_exists('scopecache_tail'));
check("function_exists scopecache_append", function_exists('scopecache_append'));

// --- 1. Fresh scope: append N items, tail them back --------------------------
$rand     = bin2hex(random_bytes(4));
$scope    = "validate-$rand";
$N        = 7;
$expected = []; // decoded payload PHP-values, in insertion order

$seqs = [];
for ($i = 0; $i < $N; $i++) {
    $payloadJson  = json_encode(['idx' => $i, 'tag' => "item-$i"]);
    $expected[]   = ['idx' => $i, 'tag' => "item-$i"];
    $env          = scopecache_append($scope, "id-$i", $payloadJson);
    check("append #$i ok=true",      ($env['ok'] ?? null) === true);
    check("append #$i created=true", ($env['created'] ?? null) === true);
    $seqs[]       = seq($env);
}

check("N appends to fresh scope returned positive seqs",
    count(array_filter($seqs, fn($s) => $s >= 1)) === $N,
    "seqs=" . json_encode($seqs));

$seqsSorted = $seqs; sort($seqsSorted);
check("appended seqs are strictly increasing",
    $seqs === $seqsSorted && count(array_unique($seqs)) === $N,
    "seqs=" . json_encode($seqs));

// --- 2. Tail returns all N items, oldest-first within the window -------------
$tail = scopecache_tail($scope, 100);
check("tail(scope, 100) returns envelope array", is_array($tail));
check("tail hit=true on populated scope", ($tail['hit'] ?? null) === true);
check("tail.count == N (=$N)", ($tail['count'] ?? -1) === $N);

$payloads = array_map(fn($it) => $it['payload'] ?? null, itemsOf($tail));
check("tail item payloads match the appended payloads in order",
    $payloads === $expected,
    "got " . json_encode($payloads));

// --- 3. Tail limit smaller than N: returns last `limit` items ----------------
$tail3 = scopecache_tail($scope, 3);
check("tail(scope, 3) count==3", ($tail3['count'] ?? -1) === 3);
$tail3Payloads = array_map(fn($it) => $it['payload'] ?? null, itemsOf($tail3));
check("tail(scope, 3) returns the newest 3 (oldest-first)",
    $tail3Payloads === array_slice($expected, $N - 3));

// --- 4. Tail with limit > N: returns all N items -----------------------------
$tail99 = scopecache_tail($scope, 99);
check("tail(scope, 99) count==N (not 99-padded)",
    ($tail99['count'] ?? -1) === $N);

// --- 5. Tail with limit=0: server clamps to default, items present -----------
// scopecache.normalizeLimit returns DefaultLimit when ?limit==0 over HTTP;
// the Gateway path treats limit=0 the same way (clamps to default). We
// just assert the call succeeds.
$tail0 = scopecache_tail($scope, 0);
check("tail(scope, 0) returns envelope", is_array($tail0));

// --- 6. Tail on a completely unknown scope: hit=false, items=[] --------------
$tail_miss = scopecache_tail("definitely-not-a-scope-$rand", 5);
check("tail(unknown-scope) hit=false",
    ($tail_miss['hit'] ?? null) === false);
check("tail(unknown-scope) items=[]", itemsOf($tail_miss) === []);

// --- 7. Read-back via scopecache_get: items written via append are visible ---
$probe = scopecache_get($scope, "id-3");
check("scopecache_get sees item-3 written via append",
    ($probe['hit'] ?? null) === true && payloadOf($probe) === $expected[3]);

// --- 8. Duplicate id: append returns error envelope --------------------------
$dup_env = scopecache_append($scope, "id-3", json_encode(['dup' => true]));
check("append of duplicate id returns error envelope (ok=false)",
    is_array($dup_env) && isErr($dup_env),
    "got " . var_export($dup_env, true));

// --- 9. Original item NOT clobbered by the failed duplicate append -----------
$probe_after_dup = scopecache_get($scope, "id-3");
check("original item-3 survived the duplicate-append rejection",
    payloadOf($probe_after_dup) === $expected[3]);

// --- 10. Invalid JSON payload: error envelope --------------------------------
$invalid_env = scopecache_append($scope, "invalid-json-id", "this is not JSON");
check("append with non-JSON payload returns error envelope",
    is_array($invalid_env) && isErr($invalid_env));

// --- 11. Empty payload: error envelope ---------------------------------------
$empty_env = scopecache_append($scope, "empty-id", "");
check("append with empty payload returns error envelope",
    is_array($empty_env) && isErr($empty_env));

// --- 12. Seq-only append (empty id): assigned seq >= 1, no id collision ------
$so_a = scopecache_append($scope, "", json_encode(['seq_only' => 'a']));
$so_b = scopecache_append($scope, "", json_encode(['seq_only' => 'b']));
check("seq-only append A ok=true + created=true",
    ($so_a['ok'] ?? null) === true && ($so_a['created'] ?? null) === true);
check("seq-only append B ok=true + created=true",
    ($so_b['ok'] ?? null) === true && ($so_b['created'] ?? null) === true);
$soSeqA = seq($so_a);
$soSeqB = seq($so_b);
check("two seq-only appends got seq >= 1", $soSeqA >= 1 && $soSeqB >= 1,
    "seqs=$soSeqA,$soSeqB");
check("the two seq-only appends got DIFFERENT seqs", $soSeqA !== $soSeqB);
check("seq-only append A has item.id=null",
    array_key_exists('id', $so_a['item']) && $so_a['item']['id'] === null);

// --- 13. After seq-only appends, tail count = N + 2 --------------------------
$tail_after = scopecache_tail($scope, 100);
check("tail after 2 seq-only appends: N+2 items present",
    ($tail_after['count'] ?? -1) === $N + 2);

// --- 14. New scope created lazily by append (no separate create call) --------
$fresh_scope = "lazily-created-$rand";
$lazy_env = scopecache_append($fresh_scope, "first", json_encode(['first' => 'item']));
check("append into never-before-seen scope created it (ok=true)",
    ($lazy_env['ok'] ?? null) === true && seq($lazy_env) >= 1);
$lazy_tail = scopecache_tail($fresh_scope, 10);
check("tail of lazily-created scope returns 1 item",
    ($lazy_tail['count'] ?? -1) === 1);

echo "\n=== SUMMARY: $pass pass, $fail fail ===\n";
if ($fail > 0) {
    echo "OVERALL: FAIL\n";
} else {
    echo "OVERALL: PASS\n";
}
