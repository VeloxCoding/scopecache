<?php
// validate-sprint2.php — correctness checks for the array-returning
// surface of the scopecache FrankenPHP extension. Every function
// (except scopecache_render_by_id/_by_seq, which mirror /render's
// raw-bytes shape) now returns a PHP associative array that mirrors
// the JSON envelope the HTTP API would produce — no json_decode
// needed.
//
// Output is PASS/FAIL per check; validate-sprint2.sh greps for the
// final OVERALL: PASS/FAIL line.

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

// Helpers to keep assertions tight. seq() pulls the assigned seq from
// an envelope produced by scopecache_append / scopecache_upsert;
// payloadOf() pulls the decoded payload from a /get envelope; itemsOf()
// pulls the items[] array from a /head or /tail envelope.
function seq(array $env): int          { return $env['item']['seq'] ?? -1; }
function payloadOf(array $env): mixed  { return $env['item']['payload'] ?? null; }
function itemsOf(array $env): array    { return $env['items'] ?? []; }

echo "=== validate-sprint2.php ===\n\n";

// --- 0. Sanity: all functions are loaded -------------------------------------
foreach ([
    'scopecache_get',
    'scopecache_get_by_seq',
    'scopecache_head',
    'scopecache_tail',
    'scopecache_render_by_id',
    'scopecache_render_by_seq',
    'scopecache_append',
    'scopecache_upsert',
    'scopecache_update',
    'scopecache_counter_add',
    'scopecache_delete',
    'scopecache_delete_by_seq',
    'scopecache_delete_up_to',
    'scopecache_delete_scope',
    'scopecache_wipe',
    'scopecache_stats',
    'scopecache_scopelist',
    'scopecache_warm',
    'scopecache_rebuild',
] as $fn) {
    check("function_exists $fn", function_exists($fn));
}

// Use a unique scope per run so leftover state from earlier sessions does
// not perturb checks.
$rand  = bin2hex(random_bytes(4));
$scope = "v2-$rand";

// --- 1. append envelope shape ------------------------------------------------
$N = 5;
$seqs = [];
for ($i = 0; $i < $N; $i++) {
    $env = scopecache_append($scope, "id-$i", json_encode(['i' => $i]));
    check("append #$i returns array envelope", is_array($env));
    check("append #$i envelope ok=true", ($env['ok'] ?? false) === true);
    check("append #$i envelope created=true", ($env['created'] ?? null) === true);
    check("append #$i envelope item.scope matches", ($env['item']['scope'] ?? '') === $scope);
    check("append #$i envelope item.id matches", ($env['item']['id'] ?? null) === "id-$i");
    check("append #$i envelope item.seq is positive int", is_int($env['item']['seq'] ?? null) && $env['item']['seq'] >= 1);
    $seqs[$i] = seq($env);
}

// --- 2. head envelope shape --------------------------------------------------
$head_all = scopecache_head($scope, 0, 100);
check("head returns array envelope", is_array($head_all));
check("head ok=true", ($head_all['ok'] ?? false) === true);
check("head hit=true (scope exists)", ($head_all['hit'] ?? null) === true);
check("head count=$N", ($head_all['count'] ?? -1) === $N);
check("head truncated=false (well under limit)", ($head_all['truncated'] ?? null) === false);
check("head items is array of $N entries", count(itemsOf($head_all)) === $N);

// Each item carries the uniform 5-key shape; payload is decoded.
$item0 = itemsOf($head_all)[0];
check("head item[0] has scope/id/seq/ts/payload keys",
    isset($item0['scope'], $item0['id'], $item0['seq'], $item0['ts'], $item0['payload']));
check("head item[0].payload is decoded to PHP array (not raw JSON string)",
    is_array($item0['payload']) && ($item0['payload']['i'] ?? null) === 0,
    "got " . var_export($item0['payload'], true));

$head_after = scopecache_head($scope, $seqs[2], 100);
check("head(after=seq[2]) returns N-3 items", count(itemsOf($head_after)) === ($N - 3));

$head_miss = scopecache_head("no-such-scope-$rand", 0, 100);
check("head(unknown scope) hit=false, items=[]",
    is_array($head_miss) && ($head_miss['hit'] ?? null) === false && itemsOf($head_miss) === []);

// --- 3. get / get_by_seq envelope --------------------------------------------
$got = scopecache_get($scope, "id-2");
check("get hit=true on existing id", ($got['hit'] ?? null) === true);
check("get count=1 on hit", ($got['count'] ?? -1) === 1);
check("get item.payload decoded to ['i' => 2]",
    payloadOf($got) === ['i' => 2],
    "got " . var_export(payloadOf($got), true));

$got_miss = scopecache_get($scope, "no-such-id-$rand");
check("get miss hit=false, count=0, item=null",
    ($got_miss['hit'] ?? null) === false
    && ($got_miss['count'] ?? -1) === 0
    && array_key_exists('item', $got_miss) && $got_miss['item'] === null);

$got_seq = scopecache_get_by_seq($scope, $seqs[2]);
check("get_by_seq hit=true on existing seq", ($got_seq['hit'] ?? null) === true);
check("get_by_seq item.payload matches", payloadOf($got_seq) === ['i' => 2]);

$got_seq_miss = scopecache_get_by_seq($scope, 9999999);
check("get_by_seq miss hit=false", ($got_seq_miss['hit'] ?? null) === false);

// --- 4. render_by_* (raw-bytes contract, unchanged) --------------------------
$render_by_id = scopecache_render_by_id($scope, "id-2");
check("render_by_id returns raw JSON-bytes string",
    $render_by_id === json_encode(['i' => 2]),
    "got " . var_export($render_by_id, true));

$render_by_seq = scopecache_render_by_seq($scope, $seqs[2]);
check("render_by_seq returns raw JSON-bytes string",
    $render_by_seq === json_encode(['i' => 2]));

// --- 5. upsert envelope ------------------------------------------------------
$ups = scopecache_upsert($scope, "id-2", json_encode(['i' => 2, 'updated' => true]));
check("upsert ok=true", ($ups['ok'] ?? null) === true);
check("upsert created=false on existing id", ($ups['created'] ?? null) === false);
check("upsert preserved seq on replace", seq($ups) === $seqs[2]);
$got_after_ups = scopecache_get($scope, "id-2");
check("upsert replaced the payload (decoded)",
    payloadOf($got_after_ups) === ['i' => 2, 'updated' => true]);

$ups_new = scopecache_upsert($scope, "id-new", json_encode(['i' => 99]));
check("upsert with new id created=true", ($ups_new['created'] ?? null) === true);
check("upsert new id seq >= 1", seq($ups_new) >= 1);

// --- 6. update envelope ------------------------------------------------------
$upd = scopecache_update($scope, "id-2", json_encode(['i' => 2, 'rewritten' => true]));
check("update ok=true", ($upd['ok'] ?? null) === true);
check("update created=false (always)", ($upd['created'] ?? null) === false);
check("update count=1 on hit", ($upd['count'] ?? -1) === 1);
$got_after_upd = scopecache_get($scope, "id-2");
check("update changed the payload",
    payloadOf($got_after_upd) === ['i' => 2, 'rewritten' => true]);

$upd_miss = scopecache_update($scope, "no-such-id-$rand", '"x"');
check("update count=0 on miss", ($upd_miss['count'] ?? -1) === 0);

// --- 7. counter_add envelope -------------------------------------------------
$counter_scope = "v2-counter-$rand";
$c1 = scopecache_counter_add($counter_scope, "hits", 1);
check("counter_add first call ok=true", ($c1['ok'] ?? null) === true);
check("counter_add first call created=true", ($c1['created'] ?? null) === true);
check("counter_add first call value=1", ($c1['value'] ?? -1) === 1);

$c2 = scopecache_counter_add($counter_scope, "hits", 5);
check("counter_add second call created=false", ($c2['created'] ?? null) === false);
check("counter_add second call value=6", ($c2['value'] ?? -1) === 6);

$c3 = scopecache_counter_add($counter_scope, "hits", -2);
check("counter_add negative value=4 (6-2)", ($c3['value'] ?? -1) === 4);

// --- 8. delete envelope ------------------------------------------------------
$del = scopecache_delete($scope, "id-0");
check("delete ok=true, hit=true, count=1",
    ($del['ok'] ?? null) === true && ($del['hit'] ?? null) === true && ($del['count'] ?? -1) === 1);
$got_after_del = scopecache_get($scope, "id-0");
check("delete actually removed the item (get hit=false)",
    ($got_after_del['hit'] ?? null) === false);

$del_miss = scopecache_delete($scope, "no-such-id-$rand");
check("delete on missing id hit=false, count=0",
    ($del_miss['hit'] ?? null) === false && ($del_miss['count'] ?? -1) === 0);

// --- 9. delete_by_seq envelope ----------------------------------------------
$del_seq = scopecache_delete_by_seq($scope, $seqs[1]);
check("delete_by_seq hit=true, count=1",
    ($del_seq['hit'] ?? null) === true && ($del_seq['count'] ?? -1) === 1);
$del_seq_miss = scopecache_delete_by_seq($scope, 9999999);
check("delete_by_seq miss hit=false", ($del_seq_miss['hit'] ?? null) === false);

// --- 10. delete_up_to envelope ----------------------------------------------
$bulk_scope = "v2-bulk-$rand";
$bulk_seqs = [];
for ($i = 0; $i < 10; $i++) {
    $bulk_seqs[$i] = seq(scopecache_append($bulk_scope, "b-$i", '"x"'));
}
$drained = scopecache_delete_up_to($bulk_scope, $bulk_seqs[4]);
check("delete_up_to count=5 (b-0..b-4)",
    ($drained['count'] ?? -1) === 5,
    "got " . var_export($drained, true));
$head_after_drain = scopecache_head($bulk_scope, 0, 100);
check("after drain, head returns 5 remaining items",
    count(itemsOf($head_after_drain)) === 5);

// --- 11. delete_scope envelope ----------------------------------------------
$ds = scopecache_delete_scope($bulk_scope);
check("delete_scope ok=true, hit=true, count=5",
    ($ds['ok'] ?? null) === true && ($ds['hit'] ?? null) === true && ($ds['count'] ?? -1) === 5);
$tail_after_drop = scopecache_tail($bulk_scope, 1);
check("after delete_scope, tail hit=false, items=[]",
    ($tail_after_drop['hit'] ?? null) === false && itemsOf($tail_after_drop) === []);

// --- 12. stats envelope (already an array, no decode) -----------------------
$stats = scopecache_stats();
check("stats returns array directly", is_array($stats));
check("stats ok=true", ($stats['ok'] ?? null) === true);
check("stats has scopes/items/approx_store_mb keys",
    isset($stats['scopes'], $stats['items'], $stats['approx_store_mb']));
check("stats.scopes is non-negative int",
    is_int($stats['scopes'] ?? null) && $stats['scopes'] >= 0);
check("stats.items is non-negative int",
    is_int($stats['items'] ?? null) && $stats['items'] >= 0);
check("stats.reserved_scopes is array of 2", count($stats['reserved_scopes'] ?? []) === 2);

// --- 13. scopelist envelope -------------------------------------------------
$list = scopecache_scopelist('', '', 100);
check("scopelist returns array directly", is_array($list));
check("scopelist ok=true", ($list['ok'] ?? null) === true);
check("scopelist.scopes is array", is_array($list['scopes'] ?? null));
$found = false;
foreach ($list['scopes'] ?? [] as $row) {
    if (($row['scope'] ?? null) === $scope) { $found = true; break; }
}
check("scopelist includes our test scope '$scope'", $found);

$list_filtered = scopecache_scopelist("v2-", '', 100);
check("scopelist with prefix 'v2-' non-empty",
    count($list_filtered['scopes'] ?? []) > 0);

// --- 14. warm envelope ------------------------------------------------------
$warm_scope_a = "v2-warm-a-$rand";
$warm_scope_b = "v2-warm-b-$rand";

$warm_input = [
    $warm_scope_a => [
        ['id' => 'one',   'payload' => json_encode(['v' => 1])],
        ['id' => 'two',   'payload' => json_encode(['v' => 2])],
        ['payload' => json_encode(['seq_only' => true])],   // no id
    ],
    $warm_scope_b => [
        ['id' => 'alpha', 'payload' => json_encode(['letter' => 'a'])],
    ],
];

$warm_env = scopecache_warm($warm_input);
check("warm ok=true, scopes=2",
    ($warm_env['ok'] ?? null) === true && ($warm_env['scopes'] ?? -1) === 2);

$warm_tail_a = scopecache_tail($warm_scope_a, 10);
check("warm A has 3 items", count(itemsOf($warm_tail_a)) === 3);

// Byte-exact round-trip per item: payload arrives decoded.
$one = scopecache_get($warm_scope_a, 'one');
check("warm A: 'one' payload decoded to ['v' => 1]",
    payloadOf($one) === ['v' => 1]);
$two = scopecache_get($warm_scope_a, 'two');
check("warm A: 'two' payload decoded to ['v' => 2]",
    payloadOf($two) === ['v' => 2]);

// Tail items[] order matches input order; each item carries its
// decoded payload AND null id for the seq-only entry.
$tailItemsA = itemsOf($warm_tail_a);
check("warm A: tail item[0].payload == ['v' => 1]",
    ($tailItemsA[0]['payload'] ?? null) === ['v' => 1]);
check("warm A: tail item[1].payload == ['v' => 2]",
    ($tailItemsA[1]['payload'] ?? null) === ['v' => 2]);
check("warm A: tail item[2] is the seq-only entry (id=null)",
    array_key_exists('id', $tailItemsA[2]) && $tailItemsA[2]['id'] === null
    && ($tailItemsA[2]['payload'] ?? null) === ['seq_only' => true]);

$warm_tail_b = scopecache_tail($warm_scope_b, 10);
check("warm B has 1 item", count(itemsOf($warm_tail_b)) === 1);
$alpha = scopecache_get($warm_scope_b, 'alpha');
check("warm B: 'alpha' payload decoded to ['letter' => 'a']",
    payloadOf($alpha) === ['letter' => 'a']);

// Cross-scope leak check (compare decoded payloads).
$tailItemsB = itemsOf($warm_tail_b);
$payloadsA = array_map(fn($it) => $it['payload'] ?? null, $tailItemsA);
$payloadsB = array_map(fn($it) => $it['payload'] ?? null, $tailItemsB);
check("warm: scope A does NOT contain scope B's payload",
    !in_array(['letter' => 'a'], $payloadsA, true));
check("warm: scope B does NOT contain scope A's payload",
    !in_array(['v' => 1], $payloadsB, true));

check("warm A: get of a non-warmed id returns hit=false",
    (scopecache_get($warm_scope_a, 'definitely-not-warmed')['hit'] ?? null) === false);

// --- 15. warm error path: missing payload -----------------------------------
$bad_warm = scopecache_warm([
    "v2-bad-$rand" => [['id' => 'a']], // payload key missing
]);
check("warm with missing payload returns error envelope",
    is_array($bad_warm) && ($bad_warm['ok'] ?? null) === false);

// --- 16. warm rejects reserved scopes ---------------------------------------
$reserved_warm = scopecache_warm([
    '_events' => [['id' => 'oops', 'payload' => '"x"']],
]);
check("warm targeting _events returns error envelope",
    is_array($reserved_warm) && ($reserved_warm['ok'] ?? null) === false);

// --- 17. rebuild envelope ---------------------------------------------------
$rebuild_scope = "v2-rebuild-$rand";
$rebuild_input = [
    $rebuild_scope => [
        ['id' => 'r1', 'payload' => '"first"'],
        ['id' => 'r2', 'payload' => '"second"'],
        ['id' => 'r3', 'payload' => '"third"'],
    ],
];

// Pre-seed an unrelated user scope to verify rebuild WIPES it.
$soon_to_be_dropped = "v2-pre-rebuild-$rand";
scopecache_append($soon_to_be_dropped, 'leftover', '"x"');
$pre_tail = scopecache_tail($soon_to_be_dropped, 1);
check("pre-rebuild: leftover scope exists", count(itemsOf($pre_tail)) === 1);

$rebuild_env = scopecache_rebuild($rebuild_input);
check("rebuild ok=true", ($rebuild_env['ok'] ?? null) === true);
check("rebuild scopes >= 1", ($rebuild_env['scopes'] ?? 0) >= 1);
check("rebuild items >= 3", ($rebuild_env['items'] ?? 0) >= 3);

$post_rebuild_dropped = scopecache_tail($soon_to_be_dropped, 1);
check("rebuild dropped the pre-existing user scope",
    ($post_rebuild_dropped['hit'] ?? null) === false);

$post_rebuild_target = scopecache_tail($rebuild_scope, 10);
check("rebuild target has 3 items",
    count(itemsOf($post_rebuild_target)) === 3);

// Byte-exact round-trip: payloads are JSON strings, decoded to PHP
// strings ("first" → 'first').
check("rebuild: r1 payload decoded to 'first'",
    payloadOf(scopecache_get($rebuild_scope, 'r1')) === 'first');
check("rebuild: r2 payload decoded to 'second'",
    payloadOf(scopecache_get($rebuild_scope, 'r2')) === 'second');
check("rebuild: r3 payload decoded to 'third'",
    payloadOf(scopecache_get($rebuild_scope, 'r3')) === 'third');

$tailItemsR = itemsOf($post_rebuild_target);
check("rebuild: tail order is r1, r2, r3 by payload",
    ($tailItemsR[0]['payload'] ?? null) === 'first'
    && ($tailItemsR[1]['payload'] ?? null) === 'second'
    && ($tailItemsR[2]['payload'] ?? null) === 'third');

$stats_after = scopecache_stats();
check("rebuild: stats.items >= 3",
    ($stats_after['items'] ?? 0) >= 3);

// --- 18. wipe envelope ------------------------------------------------------
// Run LAST because it nukes every scope.
$wipe = scopecache_wipe();
check("wipe ok=true", ($wipe['ok'] ?? null) === true);
check("wipe scopes >= 1 (something was dropped)", ($wipe['scopes'] ?? 0) >= 1);
check("wipe items >= 0", isset($wipe['items']) && is_int($wipe['items']));
check("wipe freed_mb >= 0", isset($wipe['freed_mb']) && is_float($wipe['freed_mb']));

// After wipe, test scopes must be gone.
$post_wipe_tail = scopecache_tail($scope, 1);
check("after wipe, user scope is gone (hit=false)",
    ($post_wipe_tail['hit'] ?? null) === false);
$post_wipe_counter_get = scopecache_get($counter_scope, "hits");
check("after wipe, counter scope is gone (get hit=false)",
    ($post_wipe_counter_get['hit'] ?? null) === false);

echo "\n=== SUMMARY: $pass pass, $fail fail ===\n";
if ($fail > 0) {
    echo "OVERALL: FAIL\n";
} else {
    echo "OVERALL: PASS\n";
}
