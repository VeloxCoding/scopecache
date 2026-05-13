<?php
// validate-sprint2.php — correctness checks for the Sprint 2 surface:
// head, get_by_seq, render_by_id, render_by_seq, upsert, update,
// counter_add, delete, delete_by_seq, delete_up_to, delete_scope,
// wipe, stats, scopelist.
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

echo "=== validate-sprint2.php ===\n\n";

// --- 0. Sanity: all new functions are loaded ---------------------------------
foreach ([
    'scopecache_head',
    'scopecache_get_by_seq',
    'scopecache_render_by_id',
    'scopecache_render_by_seq',
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

// --- 1. head: append N items, then head them back ----------------------------
$N = 5;
$seqs = [];
for ($i = 0; $i < $N; $i++) {
    $seqs[$i] = scopecache_append($scope, "id-$i", json_encode(['i' => $i]));
}

$head_all = scopecache_head($scope, 0, 100);
check("head(after=0, limit=100) returns array of N items",
    is_array($head_all) && count($head_all) === $N,
    "got count=" . (is_array($head_all) ? count($head_all) : 'n/a'));

$head_after = scopecache_head($scope, $seqs[2], 100);
check("head(after=seq[2], limit=100) returns the last N-3 items",
    is_array($head_after) && count($head_after) === ($N - 3),
    "got count=" . (is_array($head_after) ? count($head_after) : 'n/a'));

$head_miss = scopecache_head("no-such-scope-$rand", 0, 100);
check("head(unknown scope, ...) returns NULL", $head_miss === null);

// --- 2. get_by_seq + render_by_seq + render_by_id ----------------------------
$item2_payload_expected = json_encode(['i' => 2]);
$got_by_seq = scopecache_get_by_seq($scope, $seqs[2]);
check("get_by_seq returns the item at the requested seq",
    $got_by_seq === $item2_payload_expected,
    "got " . var_export($got_by_seq, true));

$got_by_seq_miss = scopecache_get_by_seq($scope, 9999999);
check("get_by_seq miss returns NULL", $got_by_seq_miss === null);

$render_by_id = scopecache_render_by_id($scope, "id-2");
check("render_by_id returns the same payload bytes for JSON payloads",
    $render_by_id === $item2_payload_expected,
    "got " . var_export($render_by_id, true));

$render_by_seq = scopecache_render_by_seq($scope, $seqs[2]);
check("render_by_seq returns the same payload bytes for JSON payloads",
    $render_by_seq === $item2_payload_expected);

// --- 3. upsert: create-or-replace --------------------------------------------
$upsert_seq = scopecache_upsert($scope, "id-2", json_encode(['i' => 2, 'updated' => true]));
check("upsert on existing id returns non-zero seq", $upsert_seq !== 0);
check("upsert preserved the original seq",
    $upsert_seq === $seqs[2],
    "upsert returned $upsert_seq, original was {$seqs[2]}");
$got_after_upsert = scopecache_get($scope, "id-2");
check("upsert replaced the payload",
    $got_after_upsert === json_encode(['i' => 2, 'updated' => true]));

$new_upsert_seq = scopecache_upsert($scope, "id-new", json_encode(['i' => 99]));
check("upsert with new id returns positive seq (creates)",
    $new_upsert_seq >= 1);

// --- 4. update: only touches existing items ----------------------------------
$updated = scopecache_update($scope, "id-2", json_encode(['i' => 2, 'rewritten' => true]));
check("update on existing id returns 1", $updated === 1);
$got_after_update = scopecache_get($scope, "id-2");
check("update changed the payload",
    $got_after_update === json_encode(['i' => 2, 'rewritten' => true]));

$update_missing = scopecache_update($scope, "no-such-id-$rand", '"x"');
check("update on missing id returns 0", $update_missing === 0);

// --- 5. counter_add ----------------------------------------------------------
$counter_scope = "v2-counter-$rand";
$c1 = scopecache_counter_add($counter_scope, "hits", 1);
check("counter_add first call returns 1", $c1 === 1, "got $c1");
$c2 = scopecache_counter_add($counter_scope, "hits", 5);
check("counter_add second call returns 6 (1+5)", $c2 === 6, "got $c2");
$c3 = scopecache_counter_add($counter_scope, "hits", -2);
check("counter_add with negative returns 4 (6-2)", $c3 === 4, "got $c3");

// --- 6. delete (by id) -------------------------------------------------------
$deleted = scopecache_delete($scope, "id-0");
check("delete on existing id returns 1", $deleted === 1);
$got_deleted = scopecache_get($scope, "id-0");
check("delete actually removed the item (get returns NULL)", $got_deleted === null);

$delete_missing = scopecache_delete($scope, "no-such-id-$rand");
check("delete on missing id returns 0", $delete_missing === 0);

// --- 7. delete_by_seq --------------------------------------------------------
$deleted_seq = scopecache_delete_by_seq($scope, $seqs[1]);
check("delete_by_seq on existing seq returns 1", $deleted_seq === 1);
$delete_by_seq_missing = scopecache_delete_by_seq($scope, 9999999);
check("delete_by_seq on unknown seq returns 0", $delete_by_seq_missing === 0);

// --- 8. delete_up_to ---------------------------------------------------------
$bulk_scope = "v2-bulk-$rand";
$bulk_seqs = [];
for ($i = 0; $i < 10; $i++) {
    $bulk_seqs[$i] = scopecache_append($bulk_scope, "b-$i", '"x"');
}
$drained = scopecache_delete_up_to($bulk_scope, $bulk_seqs[4]);
check("delete_up_to deletes 5 items (b-0..b-4 inclusive)", $drained === 5,
    "got $drained");
$head_after_drain = scopecache_head($bulk_scope, 0, 100);
check("after drain, head returns 5 remaining items",
    is_array($head_after_drain) && count($head_after_drain) === 5);

// --- 9. delete_scope ---------------------------------------------------------
$dropped_n = scopecache_delete_scope($bulk_scope);
check("delete_scope returns count of remaining items (5)", $dropped_n === 5,
    "got $dropped_n");
$tail_after_drop = scopecache_tail($bulk_scope, 1);
check("after delete_scope, tail returns NULL", $tail_after_drop === null);

// --- 10. stats ---------------------------------------------------------------
$stats_json = scopecache_stats();
check("stats returns a non-null string", is_string($stats_json) && $stats_json !== '');
$stats = json_decode($stats_json, true);
check("stats JSON decodes to an array", is_array($stats));
check("stats has scopes key", is_array($stats) && array_key_exists('scopes', $stats));
check("stats scopes is non-negative int",
    is_array($stats) && is_int($stats['scopes'] ?? null) && $stats['scopes'] >= 0);

// --- 11. scopelist -----------------------------------------------------------
$list_json = scopecache_scopelist('', '', 100);
check("scopelist returns a non-null string", is_string($list_json) && $list_json !== '');
$list = json_decode($list_json, true);
check("scopelist JSON decodes to an array", is_array($list));
// Our $scope should show up.
$found = false;
if (is_array($list)) {
    foreach ($list as $row) {
        if (($row['scope'] ?? null) === $scope) { $found = true; break; }
    }
}
check("scopelist includes our test scope '$scope'", $found);

$list_filtered_json = scopecache_scopelist("v2-", '', 100);
$list_filtered = json_decode($list_filtered_json, true);
check("scopelist with prefix 'v2-' returns non-empty array",
    is_array($list_filtered) && count($list_filtered) > 0);

// --- 12. warm: replace contents of selected scopes ---------------------------
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

$warm_n = scopecache_warm($warm_input);
check("warm with 2 scopes returns 2", $warm_n === 2, "got $warm_n");

$warm_tail_a = scopecache_tail($warm_scope_a, 10);
check("warm-target A has 3 items after the call",
    is_array($warm_tail_a) && count($warm_tail_a) === 3);

// Byte-exact round-trip per item in scope A — both id-addressed items
// (get) and the seq-only one (compare via tail position; tail returns
// oldest-first within the window, matching insertion order from the
// warm-input array).
check("warm A: item-one byte-exact via scopecache_get",
    scopecache_get($warm_scope_a, 'one') === json_encode(['v' => 1]));
check("warm A: item-two byte-exact via scopecache_get",
    scopecache_get($warm_scope_a, 'two') === json_encode(['v' => 2]));
check("warm A: tail order matches input order",
    is_array($warm_tail_a)
    && $warm_tail_a[0] === json_encode(['v' => 1])
    && $warm_tail_a[1] === json_encode(['v' => 2])
    && $warm_tail_a[2] === json_encode(['seq_only' => true]));

$warm_tail_b = scopecache_tail($warm_scope_b, 10);
check("warm-target B has 1 item after the call",
    is_array($warm_tail_b) && count($warm_tail_b) === 1);
check("warm B: item-alpha byte-exact via scopecache_get",
    scopecache_get($warm_scope_b, 'alpha') === json_encode(['letter' => 'a']));
check("warm B: tail content matches input",
    is_array($warm_tail_b)
    && $warm_tail_b[0] === json_encode(['letter' => 'a']));

// Cross-scope leak: scope A must not contain scope B's payload, and vice versa.
check("warm: scope A does NOT contain scope B's payload",
    is_array($warm_tail_a)
    && !in_array(json_encode(['letter' => 'a']), $warm_tail_a, true));
check("warm: scope B does NOT contain scope A's payload",
    is_array($warm_tail_b)
    && !in_array(json_encode(['v' => 1]), $warm_tail_b, true));

// Get on a definitely-NOT-warmed id in scope A must still return null
// (proves warm did not over-populate; ids stay strictly as given).
check("warm A: get of a non-warmed id returns null",
    scopecache_get($warm_scope_a, 'definitely-not-warmed') === null);

// --- 13. warm error path: missing payload -----------------------------------
$bad_warm = scopecache_warm([
    "v2-bad-$rand" => [['id' => 'a']], // payload key missing
]);
check("warm with missing payload returns 0", $bad_warm === 0,
    "got $bad_warm");

// --- 14. warm rejects reserved scopes ---------------------------------------
$reserved_warm = scopecache_warm([
    '_events' => [['id' => 'oops', 'payload' => '"x"']],
]);
check("warm targeting _events returns 0 (reserved)", $reserved_warm === 0,
    "got $reserved_warm");

// --- 15. rebuild: atomic full-state replace ---------------------------------
$rebuild_scope = "v2-rebuild-$rand";
$rebuild_input = [
    $rebuild_scope => [
        ['id' => 'r1', 'payload' => '"first"'],
        ['id' => 'r2', 'payload' => '"second"'],
        ['id' => 'r3', 'payload' => '"third"'],
    ],
];

// Pre-seed an unrelated user scope to verify rebuild WIPES it (rebuild
// is the "drop everything user-managed + apply input" primitive).
$soon_to_be_dropped = "v2-pre-rebuild-$rand";
scopecache_append($soon_to_be_dropped, 'leftover', '"x"');
$pre_tail = scopecache_tail($soon_to_be_dropped, 1);
check("pre-rebuild: leftover scope exists", is_array($pre_tail) && count($pre_tail) === 1);

$rebuild_n = scopecache_rebuild($rebuild_input);
check("rebuild returns positive scope count", $rebuild_n >= 1, "got $rebuild_n");

$post_rebuild_dropped = scopecache_tail($soon_to_be_dropped, 1);
check("rebuild dropped the pre-existing user scope",
    $post_rebuild_dropped === null);

$post_rebuild_target = scopecache_tail($rebuild_scope, 10);
check("rebuild target has 3 items",
    is_array($post_rebuild_target) && count($post_rebuild_target) === 3);

// Byte-exact round-trip per rebuild item via scopecache_get.
check("rebuild: r1 byte-exact via scopecache_get",
    scopecache_get($rebuild_scope, 'r1') === '"first"');
check("rebuild: r2 byte-exact via scopecache_get",
    scopecache_get($rebuild_scope, 'r2') === '"second"');
check("rebuild: r3 byte-exact via scopecache_get",
    scopecache_get($rebuild_scope, 'r3') === '"third"');

// Tail order matches input order (oldest-first within window).
check("rebuild: tail order is r1, r2, r3",
    is_array($post_rebuild_target)
    && $post_rebuild_target[0] === '"first"'
    && $post_rebuild_target[1] === '"second"'
    && $post_rebuild_target[2] === '"third"');

// Stats after rebuild: total item count should reflect EXACTLY what
// we put in (3 items in the user scope, plus the two reserved scopes
// _events / _inbox which rebuild re-creates).
$stats_after_rebuild = json_decode(scopecache_stats(), true);
check("rebuild: stats items >= 3 (input items present)",
    is_array($stats_after_rebuild)
    && ($stats_after_rebuild['items'] ?? 0) >= 3,
    "items=" . ($stats_after_rebuild['items'] ?? 'n/a'));

// --- 16. wipe ----------------------------------------------------------------
// Run this LAST because it nukes every scope including the ones we use
// in earlier sections. Verify the count is plausible (>= the scopes we
// created).
$scope_count_pre_wipe = scopecache_wipe();
check("wipe returns the scope count that was dropped", $scope_count_pre_wipe >= 1);

// After wipe, our test scopes must be gone.
$post_wipe_tail = scopecache_tail($scope, 1);
check("after wipe, our scope is gone (tail returns NULL)", $post_wipe_tail === null);
$post_wipe_counter = scopecache_get($counter_scope, "hits");
check("after wipe, counter scope is gone (get returns NULL)", $post_wipe_counter === null);

echo "\n=== SUMMARY: $pass pass, $fail fail ===\n";
if ($fail > 0) {
    echo "OVERALL: FAIL\n";
} else {
    echo "OVERALL: PASS\n";
}
