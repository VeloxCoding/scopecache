<?php
// Demo: FrankenPHP + scopecache + cgo PHP-extension.
//
// Each pageview:
//   1. Append a new item to scope "demo" with a random word + timestamp,
//      using scopecache_append() — direct cgo call into the in-process
//      *Gateway, no HTTP / curl.
//   2. Read the last 5 items via scopecache_tail() — same cgo path.
//   3. Render both.
//
// Refresh adds another item to the list. scopecache_* PHP functions
// are wired into the binary via the addons/frankenphp-ext addon.

// Caddy's php_server falls back to index.php for any path without a
// matching static file. Without this guard, browser auto-requests
// like /favicon.ico would re-fire scopecache_append for every page
// view. Only run the demo logic on the actual demo route.
$path = parse_url($_SERVER['REQUEST_URI'] ?? '/', PHP_URL_PATH);
if ($path !== '/' && $path !== '/index.php') {
    http_response_code(404);
    exit;
}

// Capture page-render start RIGHT at the top, before anything else
// runs (well, after the cheap path check). ob_start buffers all
// output so we can substitute the elapsed-time placeholder at the
// very end without flushing partial HTML.
$tPageStart = hrtime(true);
ob_start();

$WORDS = [
    'monkey', 'nut',   'mouse', 'tree',  'rose',   'fish',
    'fire',   'moon',  'sun',   'cloud', 'forest', 'river',
    'mountain', 'sea', 'wind',  'rain',  'snow',   'star',
];

$word = $WORDS[array_rand($WORDS)];
$ts   = (new DateTimeImmutable())->format('Y-m-d H:i:s.v');

// hrtime(true) returns monotonic nanoseconds — divide by 1000 for µs.
// Measure each cgo call individually; total time is captured at
// ob_get_clean() below.

// Append a new item. id is empty => seq-only item (cache assigns the seq).
// Extension returns the /append envelope as a JSON string; json_decode
// for PHP-side field access. The cgo call + decode are timed together
// so the displayed cost matches what a real PHP consumer pays.
$tAppend = hrtime(true);
$append_env = json_decode(scopecache_append('demo', '', json_encode([
    'word' => $word,
    'ts'   => $ts,
])), true);
$append_us = (hrtime(true) - $tAppend) / 1000;

// Read the last 5 items, newest first. Same string-return + json_decode
// pattern as append above.
$tTail = hrtime(true);
$tail_env = json_decode(scopecache_tail('demo', 5), true);
$tail_us  = (hrtime(true) - $tTail) / 1000;
$items    = $tail_env['items'] ?? [];

?>
<!doctype html>
<html lang="en">
<head>
    <meta charset="utf-8">
    <title>FrankenPHP + scopecache</title>
    <style>
        body { font-family: system-ui, sans-serif; max-width: 44em; margin: 2em auto; padding: 0 1em; color: #222; }
        h1, h2 { color: #1a1a1a; }
        code { background: #f3f3f3; padding: 0.1em 0.3em; border-radius: 3px; }
        table { border-collapse: collapse; width: 100%; margin-top: 0.5em; }
        th, td { text-align: left; padding: 0.4em 0.7em; border-bottom: 1px solid #eee; vertical-align: top; }
        th { background: #fafafa; font-weight: 600; }
        .new { font-size: 1.1em; }
        .seq { color: #666; font-family: ui-monospace, monospace; }
        .ts  { color: #666; font-family: ui-monospace, monospace; white-space: nowrap; }
        .word { font-weight: 600; }
    </style>
</head>
<body>
    <h1>Hello, scopecache</h1>

    <p class="new">
        Just appended: <span class="word"><?= htmlspecialchars($word) ?></span>
        at <span class="ts"><?= htmlspecialchars($ts) ?></span>
        (seq <code><?= htmlspecialchars((string)($append_env['item']['seq'] ?? '?')) ?></code>).
    </p>

    <p>
        Every refresh appends a new random word + timestamp to scope
        <code>demo</code> via <code>scopecache_append()</code>, then reads
        the last 5 items via <code>scopecache_tail()</code>. Both calls
        are direct cgo into the in-process <code>*Gateway</code> — no HTTP,
        no curl, no JSON over the wire.
    </p>

    <h2>Timings (this request)</h2>
    <table>
        <tbody>
            <tr>
                <td><code>scopecache_append()</code></td>
                <td class="ts"><?= number_format($append_us, 1) ?> µs</td>
            </tr>
            <tr>
                <td><code>scopecache_tail(limit=5)</code></td>
                <td class="ts"><?= number_format($tail_us, 1) ?> µs</td>
            </tr>
            <tr>
                <td><strong>Whole PHP page render</strong> (start → output flush)</td>
                <td class="ts" id="total-cell"><strong><span id="total">…</span> µs</strong></td>
            </tr>
        </tbody>
    </table>
    <p style="font-size: 0.9em; color: #666;">
        Both cgo calls are sub-microsecond on a warm runtime. The whole
        page-render total includes PHP parsing, the cgo calls, JSON
        encoding/decoding, HTML rendering, and the trailing flush —
        but no HTTP-roundtrip overhead because everything is in-process.
    </p>

    <h2>Last 5 items in scope <code>demo</code></h2>

    <?php if (!$items): ?>
        <p>(scope is empty — refresh again to see your first append appear here)</p>
    <?php else: ?>
        <table>
            <thead>
                <tr><th>seq</th><th>word</th><th>timestamp</th></tr>
            </thead>
            <tbody>
                <?php foreach ($items as $item): ?>
                    <?php
                        $payload = $item['payload'] ?? [];
                        $w = $payload['word'] ?? '?';
                        $t = $payload['ts']   ?? '?';
                    ?>
                    <tr>
                        <td class="seq"><?= htmlspecialchars((string)($item['seq'] ?? '?')) ?></td>
                        <td class="word"><?= htmlspecialchars($w) ?></td>
                        <td class="ts"><?= htmlspecialchars($t) ?></td>
                    </tr>
                <?php endforeach ?>
            </tbody>
        </table>
    <?php endif ?>

    <h2>Talk to the cache directly</h2>
    <ul>
        <li><a href="/stats">/stats</a> — JSON snapshot of the whole cache</li>
        <li><a href="/tail?scope=demo&amp;limit=10">/tail?scope=demo</a> — same items, JSON envelope</li>
        <li><a href="/scopelist">/scopelist</a> — list of every scope</li>
    </ul>

    <p>
        The cache lives only in this process's memory. Restart the binary
        (or hit <code>/wipe</code>) to clear it.
    </p>
</body>
</html>
<?php
// Compute the whole-page elapsed time NOW (after all rendering) and
// substitute it into the buffered HTML before flushing. The placeholder
// in the page is <span id="total">…</span>; we replace its content
// with the µs value.
$total_us = (hrtime(true) - $tPageStart) / 1000;
$html = ob_get_clean();
echo str_replace(
    '<span id="total">…</span>',
    number_format($total_us, 1),
    $html
);

