# FrankenPHP + scopecache — standalone binary

A fully-static, musl-linked Linux binary that bundles:

- Caddy web server (port 8080 by default)
- FrankenPHP runtime (PHP 8 ZTS, baked in)
- scopecache as a Caddy module (`/get`, `/append`, `/stats`, …)
- A minimal hello-world PHP page

No package installs, no Docker, no system PHP, no shared libraries.
Drop it on a Linux server, make it executable, run it.

## Quick start

### On Linux (any distro, x86_64)

```bash
chmod +x frankenphp-static-linux-x86_64
./frankenphp-static-linux-x86_64 php-server
```

The `php-server` subcommand is FrankenPHP's; it boots the **embedded**
Caddyfile + PHP app baked into the binary. Plain `caddy run` without
`--config` won't work — there's no Caddyfile on disk to point at.

### On Windows / macOS / anywhere with Docker

The binary is Linux-only, but Docker Desktop runs a Linux VM under the
hood. Wrap the binary in a minimal Alpine container:

```bash
docker run -d --name fpbin -p 8080:8080 \
    -v "$(pwd):/app:ro" \
    --entrypoint /app/frankenphp-static-linux-x86_64 \
    alpine:latest php-server
```

On Git-Bash for Windows prefix with `MSYS_NO_PATHCONV=1` or it will
rewrite `/app` to a Windows path before Docker sees it. If port 8080
is already taken (common in dev environments), change `-p 8080:8080`
to `-p 8090:8080` and open `http://localhost:8090/`.

Stop with `docker rm -f fpbin`.

### What the demo does

Open `http://localhost:8080/` (or whichever port you mapped).

Every refresh:

1. Picks a random Dutch noun.
2. Calls `scopecache_append('demo', '', json_encode(['word'=>..., 'ts'=>...]))`
   — **direct cgo into the in-process `*Gateway`**, no HTTP.
3. Calls `scopecache_tail('demo', 10)` and renders the last 10 items
   in a table.

So the seq counter grows each refresh, the table fills with new
words, the timestamps show real wall-clock spacing.

The page links to a few cache endpoints you can also hit directly:

- `/stats` — JSON snapshot of the whole cache
- `/tail?scope=demo&limit=10` — same items, JSON envelope
- `/scopelist` — list of every scope (you'll see `_events`, `_inbox`, `demo`)
- `POST /wipe` — clear the cache without restarting the binary

## What the binary actually is

Run `file` against it to confirm:

```text
frankenphp-static-linux-x86_64: ELF 64-bit LSB executable, x86-64,
  statically linked, stripped
```

Statically linked = no `libc.so`, no `libssl.so`, no `libphp.so`
needed on the host. Runs on Alpine, Debian, Ubuntu, Arch, RHEL,
Amazon Linux, anything with a Linux kernel ≥ 3.2 and x86_64 CPU.

## Running it as a service (systemd)

`/etc/systemd/system/scopecache.service`:

```ini
[Unit]
Description=FrankenPHP + scopecache
After=network.target

[Service]
Type=simple
ExecStart=/opt/scopecache/frankenphp-static-linux-x86_64 php-server
Restart=always
User=www-data

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now scopecache
```

## Customising at runtime

The binary's default Caddyfile + PHP files are **baked in**. To
override either:

### Use a different Caddyfile

```bash
./frankenphp-static-linux-x86_64 run --config /etc/my.Caddyfile
```

Your Caddyfile should set `root` to a real directory containing
your PHP files:

```caddyfile
{
    auto_https off
    order scopecache before php_server
}

:8080 {
    root * /var/www/html
    scopecache {
        scope_max_items 10000
        max_store_mb    256
    }
    php_server
}
```

### Bind a different port

Edit the Caddyfile's `:8080` site address (or run the binary with
a `--config` flag pointing at one that uses `:80`, `:443`, etc.).

### Open the admin API

The embedded Caddyfile has `admin off`. If you want to use Caddy's
config-reload endpoint (`POST /load`), drop the `admin off` line in
your override Caddyfile.

## Building a fresh binary

See [`tools/frankenphp-bin/`](../../tools/frankenphp-bin/) for the
build script + the full list of build-chain pitfalls. Short version:

```bash
cd tools/frankenphp-bin
./build.sh        # ~15-45 min, output lands here
```

## Where this binary differs from `cmd/scopecache`

| | `frankenphp-static-linux-x86_64` | `cmd/scopecache` |
|---|---|---|
| What it is | Caddy + FrankenPHP + scopecache in one binary | Just scopecache, no Caddy, no PHP |
| Transport | HTTP on TCP (`:8080`) | Unix socket |
| Includes PHP runtime | Yes — also serves PHP files | No — pair with FrankenPHP or PHP-FPM separately |
| Sub-package | `caddymodule/` (this binary uses it) | `package scopecache` directly |
| Use case | Single-binary VPS deployment | Run behind nginx/apache, or talk to it over the socket |
