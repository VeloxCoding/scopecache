<?php
// Demo: FrankenPHP + scopecache + cgo PHP-extension.
//
// Each pageview:
//   1. Append a new item to scope "demo" with a random word + timestamp,
//      using scopecache_append() — direct cgo call into the in-process
//      *Gateway, no HTTP / curl.
//   2. Read the last 10 items via scopecache_tail() — same cgo path.
//   3. Render both.
//
// Refresh adds another item to the list. scopecache_* PHP functions
// are wired into the binary via the addons/frankenphp-ext addon.

$WORDS = [
    'aap', 'noot', 'mies', 'boom', 'roos', 'vis',
    'vuur', 'maan', 'zon', 'wolk', 'bos', 'rivier',
    'berg', 'zee', 'wind', 'regen', 'sneeuw', 'ster',
];

$word = $WORDS[array_rand($WORDS)];
$ts   = (new DateTimeImmutable())->format('Y-m-d H:i:s.v');

// Append a new item. id is empty => seq-only item (cache assigns the seq).
$append_env = scopecache_append('demo', '', json_encode([
    'word' => $word,
    'ts'   => $ts,
]));

// Read the last 10 items, newest first.
$tail_env = scopecache_tail('demo', 10);
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
        the last 10 items via <code>scopecache_tail()</code>. Both calls
        are direct cgo into the in-process <code>*Gateway</code> — no HTTP,
        no curl, no JSON over the wire.
    </p>

    <h2>Last 10 items in scope <code>demo</code></h2>

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
