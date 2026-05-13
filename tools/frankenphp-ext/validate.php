<?php
// validate.php — full correctness suite for the scopecache FrankenPHP
// extension. Every //export_php function gets:
//   - envelope-shape assertions ({ok}, {hit}, {created}, etc.)
//   - happy-path round-trip (decoded payload matches what we put in)
//   - relevant edge cases (miss, duplicate, invalid input)
//
// Output is PASS/FAIL per check; validate.sh greps for the final
// OVERALL: PASS/FAIL line.

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

function section(string $title): void {
    echo "\n--- $title " . str_repeat('-', max(0, 60 - strlen($title))) . "\n";
}

// Envelope-shape helpers — used everywhere below.
function seq(array $env): int         { return $env['item']['seq'] ?? -1; }
function payloadOf(array $env): mixed { return $env['item']['payload'] ?? null; }
function itemsOf(array $env): array   { return $env['items'] ?? []; }
function isErr(array $env): bool      { return ($env['ok'] ?? null) === false; }

echo "=== validate.php — scopecache FrankenPHP extension ===\n";

// --- 0. Sanity: every extension function is registered ----------------------
section('0. functions registered');
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

// Unique scope per run so prior state cannot perturb checks.
$rand          = bin2hex(random_bytes(4));
$scope         = "v-$rand";
$counter_scope = "v-counter-$rand";

// --- 1. append: envelope shape + edge cases --------------------------------
section('1. append');

$N        = 7;
$expected = []; // decoded payload PHP-values, in insertion order
$seqs     = [];
for ($i = 0; $i < $N; $i++) {
    $payloadJson = json_encode(['idx' => $i, 'tag' => "item-$i"]);
    $expected[]  = ['idx' => $i, 'tag' => "item-$i"];
    $env         = scopecache_append($scope, "id-$i", $payloadJson);
    check("append #$i ok=true",                ($env['ok'] ?? null) === true);
    check("append #$i created=true",           ($env['created'] ?? null) === true);
    check("append #$i item.scope matches",     ($env['item']['scope'] ?? null) === $scope);
    check("append #$i item.id matches",        ($env['item']['id'] ?? null) === "id-$i");
    check("append #$i item.seq positive int",  is_int($env['item']['seq'] ?? null) && $env['item']['seq'] >= 1);
    $seqs[] = seq($env);
}

$seqsSorted = $seqs; sort($seqsSorted);
check("appended seqs are strictly increasing",
    $seqs === $seqsSorted && count(array_unique($seqs)) === $N,
    "seqs=" . json_encode($seqs));

// Duplicate id rejected as error envelope, original survives.
$dup = scopecache_append($scope, "id-3", json_encode(['dup' => true]));
check("append duplicate id returns error envelope", is_array($dup) && isErr($dup));
$probe_after_dup = scopecache_get($scope, "id-3");
check("original item-3 survived the duplicate rejection",
    payloadOf($probe_after_dup) === $expected[3]);

// Invalid / empty payloads rejected as error envelope.
$bad_json = scopecache_append($scope, "invalid-json-id", "this is not JSON");
check("append non-JSON payload returns error envelope", isErr($bad_json));
$empty = scopecache_append($scope, "empty-id", "");
check("append empty payload returns error envelope",   isErr($empty));

// Seq-only append: empty id, assigned seq, id=null in envelope.
$so_a = scopecache_append($scope, "", json_encode(['seq_only' => 'a']));
$so_b = scopecache_append($scope, "", json_encode(['seq_only' => 'b']));
check("seq-only append A created=true",         ($so_a['created'] ?? null) === true);
check("seq-only append B created=true",         ($so_b['created'] ?? null) === true);
check("seq-only appends got distinct seqs >=1",
    seq($so_a) >= 1 && seq($so_b) >= 1 && seq($so_a) !== seq($so_b));
check("seq-only append A has item.id = null",
    array_key_exists('id', $so_a['item']) && $so_a['item']['id'] === null);

// Lazy scope creation.
$fresh_scope = "lazy-$rand";
$lazy = scopecache_append($fresh_scope, "first", json_encode(['first' => 'item']));
check("append into never-seen scope creates it (ok=true, seq>=1)",
    ($lazy['ok'] ?? null) === true && seq($lazy) >= 1);

// --- 2. get / get_by_seq ---------------------------------------------------
section('2. get + get_by_seq');

$got = scopecache_get($scope, "id-3");
check("get hit=true",                            ($got['hit'] ?? null) === true);
check("get count=1 on hit",                      ($got['count'] ?? -1) === 1);
check("get item.payload decoded to PHP array",   payloadOf($got) === $expected[3]);

$got_miss = scopecache_get($scope, "no-such-id-$rand");
check("get miss hit=false, count=0, item=null",
    ($got_miss['hit'] ?? null) === false
    && ($got_miss['count'] ?? -1) === 0
    && array_key_exists('item', $got_miss) && $got_miss['item'] === null);

$got_seq = scopecache_get_by_seq($scope, $seqs[2]);
check("get_by_seq hit=true",                    ($got_seq['hit'] ?? null) === true);
check("get_by_seq item.payload matches",        payloadOf($got_seq) === $expected[2]);

$got_seq_miss = scopecache_get_by_seq($scope, 9999999);
check("get_by_seq miss hit=false",              ($got_seq_miss['hit'] ?? null) === false);

// --- 3. head ----------------------------------------------------------------
section('3. head');

// Note: $scope now has N + 2 items (N initial + 2 seq-only).
$head_all = scopecache_head($scope, 0, 100);
check("head ok=true",                           ($head_all['ok'] ?? null) === true);
check("head hit=true on populated scope",       ($head_all['hit'] ?? null) === true);
check("head count == N + 2 (incl. seq-only)",   ($head_all['count'] ?? -1) === $N + 2);
check("head truncated=false (well under limit)",($head_all['truncated'] ?? null) === false);

$item0 = itemsOf($head_all)[0];
check("head item[0] has scope/id/seq/ts/payload keys",
    isset($item0['scope'], $item0['id'], $item0['seq'], $item0['ts'], $item0['payload']));
check("head item[0].payload is decoded PHP array",
    is_array($item0['payload']) && $item0['payload'] === $expected[0]);

$head_after = scopecache_head($scope, $seqs[2], 100);
check("head(after=seq[2]) returns items past that seq",
    count(itemsOf($head_after)) === ($N + 2 - 3));

$head_miss = scopecache_head("no-such-scope-$rand", 0, 100);
check("head(unknown scope) hit=false, items=[]",
    ($head_miss['hit'] ?? null) === false && itemsOf($head_miss) === []);

// --- 4. tail ----------------------------------------------------------------
section('4. tail');

$tail = scopecache_tail($scope, 100);
check("tail hit=true",                          ($tail['hit'] ?? null) === true);
check("tail count == N + 2",                    ($tail['count'] ?? -1) === $N + 2);

$tail_payloads = array_map(fn($it) => $it['payload'] ?? null, itemsOf($tail));
$expected_with_seqonly = array_merge($expected, [['seq_only' => 'a'], ['seq_only' => 'b']]);
check("tail item payloads match insertion order",
    $tail_payloads === $expected_with_seqonly);

// Smaller limit returns newest N within window (oldest-first inside).
$tail3 = scopecache_tail($scope, 3);
check("tail(scope, 3) count==3", ($tail3['count'] ?? -1) === 3);
$tail3_payloads = array_map(fn($it) => $it['payload'] ?? null, itemsOf($tail3));
check("tail(scope, 3) returns the newest 3",
    $tail3_payloads === array_slice($expected_with_seqonly, $N + 2 - 3));

// limit > N still gives N (no padding).
$tail99 = scopecache_tail($scope, 99);
check("tail(scope, 99) count==N+2 (not 99-padded)",
    ($tail99['count'] ?? -1) === $N + 2);

// Unknown scope: hit=false, items=[].
$tail_miss = scopecache_tail("no-such-scope-$rand", 5);
check("tail(unknown scope) hit=false",          ($tail_miss['hit'] ?? null) === false);
check("tail(unknown scope) items=[]",           itemsOf($tail_miss) === []);

// --- 5. render_by_id / _by_seq ---------------------------------------------
section('5. render');

$expected_3_json = json_encode($expected[3]);
$render_by_id    = scopecache_render_by_id($scope, "id-3");
check("render_by_id returns raw JSON bytes",
    $render_by_id === $expected_3_json,
    "got " . var_export($render_by_id, true));

$render_by_seq   = scopecache_render_by_seq($scope, $seqs[3]);
check("render_by_seq returns raw JSON bytes",
    $render_by_seq === $expected_3_json);

// --- 6. upsert -------------------------------------------------------------
section('6. upsert');

// Replace existing.
$ups = scopecache_upsert($scope, "id-3", json_encode(['idx' => 3, 'updated' => true]));
check("upsert ok=true",                         ($ups['ok'] ?? null) === true);
check("upsert created=false on existing id",    ($ups['created'] ?? null) === false);
check("upsert preserved seq on replace",        seq($ups) === $seqs[3]);
check("upsert replaced the payload (decoded)",
    payloadOf(scopecache_get($scope, "id-3")) === ['idx' => 3, 'updated' => true]);

// Create new.
$ups_new = scopecache_upsert($scope, "id-new", json_encode(['fresh' => true]));
check("upsert new id created=true",             ($ups_new['created'] ?? null) === true);
check("upsert new id seq>=1",                   seq($ups_new) >= 1);

// --- 7. update -------------------------------------------------------------
section('7. update');

$upd = scopecache_update($scope, "id-3", json_encode(['idx' => 3, 'rewritten' => true]));
check("update ok=true",                         ($upd['ok'] ?? null) === true);
check("update created=false (always)",          ($upd['created'] ?? null) === false);
check("update count=1 on hit",                  ($upd['count'] ?? -1) === 1);
check("update changed the payload",
    payloadOf(scopecache_get($scope, "id-3")) === ['idx' => 3, 'rewritten' => true]);

$upd_miss = scopecache_update($scope, "no-such-id-$rand", '"x"');
check("update count=0 on miss",                 ($upd_miss['count'] ?? -1) === 0);

// --- 8. counter_add --------------------------------------------------------
section('8. counter_add');

$c1 = scopecache_counter_add($counter_scope, "hits", 1);
check("counter_add first call created=true",    ($c1['created'] ?? null) === true);
check("counter_add first call value=1",         ($c1['value'] ?? -1) === 1);

$c2 = scopecache_counter_add($counter_scope, "hits", 5);
check("counter_add second call created=false",  ($c2['created'] ?? null) === false);
check("counter_add second call value=6",        ($c2['value'] ?? -1) === 6);

$c3 = scopecache_counter_add($counter_scope, "hits", -2);
check("counter_add negative -> value=4 (6-2)",  ($c3['value'] ?? -1) === 4);

// --- 9. delete + delete_by_seq ---------------------------------------------
section('9. delete + delete_by_seq');

$del = scopecache_delete($scope, "id-0");
check("delete hit=true, count=1",
    ($del['hit'] ?? null) === true && ($del['count'] ?? -1) === 1);
check("delete actually removed the item",
    (scopecache_get($scope, "id-0")['hit'] ?? null) === false);

$del_miss = scopecache_delete($scope, "no-such-id-$rand");
check("delete miss: hit=false, count=0",
    ($del_miss['hit'] ?? null) === false && ($del_miss['count'] ?? -1) === 0);

$del_seq = scopecache_delete_by_seq($scope, $seqs[1]);
check("delete_by_seq hit=true, count=1",
    ($del_seq['hit'] ?? null) === true && ($del_seq['count'] ?? -1) === 1);

$del_seq_miss = scopecache_delete_by_seq($scope, 9999999);
check("delete_by_seq miss hit=false",           ($del_seq_miss['hit'] ?? null) === false);

// --- 10. delete_up_to ------------------------------------------------------
section('10. delete_up_to');

$bulk_scope = "bulk-$rand";
$bulk_seqs  = [];
for ($i = 0; $i < 10; $i++) {
    $bulk_seqs[$i] = seq(scopecache_append($bulk_scope, "b-$i", '"x"'));
}
$drained = scopecache_delete_up_to($bulk_scope, $bulk_seqs[4]);
check("delete_up_to count=5 (b-0..b-4)",        ($drained['count'] ?? -1) === 5);
check("after drain, head returns 5 remaining items",
    count(itemsOf(scopecache_head($bulk_scope, 0, 100))) === 5);

// --- 11. delete_scope ------------------------------------------------------
section('11. delete_scope');

$ds = scopecache_delete_scope($bulk_scope);
check("delete_scope hit=true, count=5",
    ($ds['hit'] ?? null) === true && ($ds['count'] ?? -1) === 5);
check("after delete_scope, tail hit=false items=[]",
    (scopecache_tail($bulk_scope, 1)['hit'] ?? null) === false
    && itemsOf(scopecache_tail($bulk_scope, 1)) === []);

// --- 12. stats -------------------------------------------------------------
section('12. stats');

$stats = scopecache_stats();
check("stats ok=true",                          ($stats['ok'] ?? null) === true);
check("stats has scopes/items/approx_store_mb",
    isset($stats['scopes'], $stats['items'], $stats['approx_store_mb']));
check("stats.scopes >= 0 (int)",                is_int($stats['scopes'] ?? null) && $stats['scopes'] >= 0);
check("stats.items  >= 0 (int)",                is_int($stats['items']  ?? null) && $stats['items']  >= 0);
check("stats.reserved_scopes is array of 2",    count($stats['reserved_scopes'] ?? []) === 2);

// --- 13. scopelist ---------------------------------------------------------
section('13. scopelist');

$list = scopecache_scopelist('', '', 100);
check("scopelist ok=true",                      ($list['ok'] ?? null) === true);
check("scopelist.scopes is array",              is_array($list['scopes'] ?? null));
$found = false;
foreach ($list['scopes'] ?? [] as $row) {
    if (($row['scope'] ?? null) === $scope) { $found = true; break; }
}
check("scopelist includes our test scope '$scope'", $found);

$list_filtered = scopecache_scopelist("v-", '', 100);
check("scopelist prefix='v-' returns non-empty",
    count($list_filtered['scopes'] ?? []) > 0);

// --- 14. warm --------------------------------------------------------------
section('14. warm');

$warm_a = "warm-a-$rand";
$warm_b = "warm-b-$rand";
$warm_input = [
    $warm_a => [
        ['id' => 'one',   'payload' => json_encode(['v' => 1])],
        ['id' => 'two',   'payload' => json_encode(['v' => 2])],
        ['payload' => json_encode(['seq_only' => true])],
    ],
    $warm_b => [
        ['id' => 'alpha', 'payload' => json_encode(['letter' => 'a'])],
    ],
];

$warm = scopecache_warm($warm_input);
check("warm ok=true, scopes=2",
    ($warm['ok'] ?? null) === true && ($warm['scopes'] ?? -1) === 2);

$tail_a = scopecache_tail($warm_a, 10);
check("warm A has 3 items",                     count(itemsOf($tail_a)) === 3);
check("warm A: 'one' decoded to ['v'=>1]",      payloadOf(scopecache_get($warm_a, 'one')) === ['v' => 1]);
check("warm A: 'two' decoded to ['v'=>2]",      payloadOf(scopecache_get($warm_a, 'two')) === ['v' => 2]);

$tail_a_items = itemsOf($tail_a);
check("warm A: tail[0].payload == ['v'=>1]",    ($tail_a_items[0]['payload'] ?? null) === ['v' => 1]);
check("warm A: tail[1].payload == ['v'=>2]",    ($tail_a_items[1]['payload'] ?? null) === ['v' => 2]);
check("warm A: tail[2] is seq-only (id=null)",
    array_key_exists('id', $tail_a_items[2]) && $tail_a_items[2]['id'] === null
    && ($tail_a_items[2]['payload'] ?? null) === ['seq_only' => true]);

$tail_b = scopecache_tail($warm_b, 10);
check("warm B has 1 item",                      count(itemsOf($tail_b)) === 1);
check("warm B: 'alpha' decoded",                payloadOf(scopecache_get($warm_b, 'alpha')) === ['letter' => 'a']);

// Cross-scope leak check.
$payloads_a = array_map(fn($it) => $it['payload'] ?? null, $tail_a_items);
$payloads_b = array_map(fn($it) => $it['payload'] ?? null, itemsOf($tail_b));
check("warm: scope A does NOT contain scope B's payload",
    !in_array(['letter' => 'a'], $payloads_a, true));
check("warm: scope B does NOT contain scope A's payload",
    !in_array(['v' => 1], $payloads_b, true));

// Error envelopes for bad input.
$bad_warm = scopecache_warm(["bad-$rand" => [['id' => 'a']]]); // missing payload
check("warm with missing payload returns error envelope",
    isErr($bad_warm));

$reserved_warm = scopecache_warm(['_events' => [['id' => 'oops', 'payload' => '"x"']]]);
check("warm targeting _events returns error envelope",
    isErr($reserved_warm));

// --- 15. rebuild -----------------------------------------------------------
section('15. rebuild');

$rebuild_scope = "rebuild-$rand";
$rebuild_input = [
    $rebuild_scope => [
        ['id' => 'r1', 'payload' => '"first"'],
        ['id' => 'r2', 'payload' => '"second"'],
        ['id' => 'r3', 'payload' => '"third"'],
    ],
];

// Pre-seed an unrelated scope to verify rebuild drops it.
$leftover_scope = "pre-rebuild-$rand";
scopecache_append($leftover_scope, 'leftover', '"x"');
check("pre-rebuild: leftover scope exists",
    count(itemsOf(scopecache_tail($leftover_scope, 1))) === 1);

$rebuild = scopecache_rebuild($rebuild_input);
check("rebuild ok=true",                        ($rebuild['ok'] ?? null) === true);
check("rebuild scopes >= 1",                    ($rebuild['scopes'] ?? 0) >= 1);
check("rebuild items >= 3",                     ($rebuild['items'] ?? 0) >= 3);

check("rebuild dropped the pre-existing scope",
    (scopecache_tail($leftover_scope, 1)['hit'] ?? null) === false);

$target_tail = scopecache_tail($rebuild_scope, 10);
check("rebuild target has 3 items",             count(itemsOf($target_tail)) === 3);

// JSON-string payloads decode to PHP strings.
check("rebuild: r1 decoded to 'first'",         payloadOf(scopecache_get($rebuild_scope, 'r1')) === 'first');
check("rebuild: r2 decoded to 'second'",        payloadOf(scopecache_get($rebuild_scope, 'r2')) === 'second');
check("rebuild: r3 decoded to 'third'",         payloadOf(scopecache_get($rebuild_scope, 'r3')) === 'third');

$target_items = itemsOf($target_tail);
check("rebuild: tail order is r1, r2, r3",
    ($target_items[0]['payload'] ?? null) === 'first'
    && ($target_items[1]['payload'] ?? null) === 'second'
    && ($target_items[2]['payload'] ?? null) === 'third');

// --- 16. wipe (LAST — nukes everything) -----------------------------------
section('16. wipe');

$wipe = scopecache_wipe();
check("wipe ok=true",                           ($wipe['ok'] ?? null) === true);
check("wipe scopes >= 1 (something was dropped)", ($wipe['scopes'] ?? 0) >= 1);
check("wipe items is int",                      isset($wipe['items']) && is_int($wipe['items']));
check("wipe freed_mb is float",                 isset($wipe['freed_mb']) && is_float($wipe['freed_mb']));

check("after wipe, every test scope is gone (rebuild target)",
    (scopecache_tail($rebuild_scope, 1)['hit'] ?? null) === false);
check("after wipe, counter scope is gone",
    (scopecache_get($counter_scope, "hits")['hit'] ?? null) === false);

// ---------------------------------------------------------------------------
echo "\n=== SUMMARY: $pass pass, $fail fail ===\n";
echo $fail > 0 ? "OVERALL: FAIL\n" : "OVERALL: PASS\n";
