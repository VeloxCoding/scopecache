# FrankenPHP + scopecache static-binary builder

Produces a single fully-static Linux binary that bundles:

- **FrankenPHP** (Caddy web server + embedded PHP runtime)
- **scopecache** (this repo) as a Caddy module
- **`addons/guarded`** — bearer-token-gated `/tail` (wired in by caddymodule)
- **`addons/frankenphp-ext`** — PHP extension; `scopecache_*()` calls
  in PHP go directly into the in-process `*Gateway` via cgo, bypassing
  any HTTP round-trip
- **An embedded PHP app + Caddyfile** ([`embed/`](embed/)) baked into
  the binary's filesystem

The output is musl-linked, has no glibc or shared-library dependencies,
and runs on any modern x86_64 / arm64 Linux machine — drop it on a VPS,
make it executable, run it. No package installs, no Docker required at
runtime.

## Quick start

1. **Get a GitHub Personal Access Token.** Generate one at
   [github.com/settings/tokens](https://github.com/settings/tokens) →
   "Generate new token (classic)". **No scopes needed** — leave every
   checkbox blank. The token only exists to raise your github API
   rate-limit from 60/h (anonymous) to 5000/h (authenticated), which
   `spc` needs to download PHP + every extension's source mid-build.
   Without it the build dies around minute 15. Token starts with `ghp_`.

2. **Export the token + run the build.** From the repo root:

   ```bash
   export GITHUB_TOKEN=ghp_xxxxxxxxxxxxxxxxxxxxxxxx
   cd tools/frankenphp-bin
   ./build.sh
   ```

   Cold: ~15-45 min (compiles PHP + every extension from source).
   Warm (docker layer cache): ~3-10 min.

3. **Use the produced binary.** Lands at
   [`examples/frankenphp-bin/frankenphp-static-linux-<arch>`](../../examples/frankenphp-bin/).
   See that directory's README for run instructions.

After the build, revoke the token if it was scoped just for this run —
it now exists on your `~/.bash_history` and any logs that captured the
build session.

## What lives here

| file | role |
|---|---|
| [`build.sh`](build.sh) | Orchestrator. Three stages: (0) pre-generates the PHP-extension C wrappers in a separate generator container, (1) clones FrankenPHP + stages scopecache source, (2) runs `docker buildx bake static-builder-musl` and extracts the resulting binary. |
| [`embed/Caddyfile`](embed/Caddyfile) | The Caddyfile baked into the binary via `EMBED=./embed`. Override at runtime by passing `--config /path/to/your.Caddyfile`. |
| [`embed/public/index.php`](embed/public/index.php) | Default demo page baked in. Appends a random word + timestamp on every request via `scopecache_append()` and reads the last 10 items via `scopecache_tail()` — both direct cgo calls into the in-process `*Gateway`. |

The `embed/` content is the binary's default. Override either piece at
runtime by passing a Caddyfile that serves a different `root`.

The PHP-extension generator image (`frankenphp-ext-builder:latest`)
is built from [`../frankenphp-ext/Dockerfile.gen`](../frankenphp-ext/Dockerfile.gen)
and reused across both static and dynamic builds.

## Configurable inputs

```bash
FRANKENPHP_VERSION=v1.12.2 ./build.sh
PHP_EXTENSIONS=curl,opcache ./build.sh
GITHUB_TOKEN=ghp_... ./build.sh
```

| env var | default | purpose |
|---|---|---|
| `FRANKENPHP_VERSION` | `v1.12.2` | FrankenPHP tag to clone + bake against |
| `PHP_EXTENSIONS` | `curl,opcache,openssl,mbstring,sodium,pdo,pdo_sqlite,session,tokenizer,filter,ctype,iconv` | comma-separated list passed to spc |
| `GITHUB_TOKEN` | (unset) | passed to the static-php-cli pipeline to avoid github API rate-limits during the build |

## Build-chain pitfalls

These survived a build session that fell over on each one. The
workarounds are baked into [`build.sh`](build.sh); these notes explain
**why** each line is there.

### 1. PHP extension needs a pre-generation step (Stage 0)

The `static-builder-musl` pipeline runs `xcaddy build` directly against
the source you point `--with` at. It has no knowledge of FrankenPHP's
`//export_php:` directive — that's a `frankenphp-gen extension-init`
concept that runs as a **separate** Go-source-rewriting step, producing
C wrappers in a `build/` subdirectory next to the original `.go` file.

If you `--with` the extension source as-is, xcaddy compiles a Go
package with `// export_php:` comments and no C wrappers — link
fails, the `scopecache_*` PHP functions are missing.

**Workaround:** [`build.sh`](build.sh)'s Stage 0 runs
`frankenphp-gen extension-init` in the same generator image
[`tools/frankenphp-ext/`](../frankenphp-ext/) uses, plus the two
sed-patches that tooling already applies. Output gets staged into the
build context **before** static-builder-musl runs, so xcaddy sees a
fully-generated extension source tree.

For the underlying pitfalls of the extension itself (gofmt-rewriting,
`RETURN_EMPTY_*` patches, UAF on string keys, etc.) see
[`tools/frankenphp-ext/README.md`](../frankenphp-ext/README.md). The
ones that affect Stage 0 are handled inside build.sh; the ones that
are source-conventions are handled in
[`addons/frankenphp-ext/scopecache_ext.go`](../../addons/frankenphp-ext/scopecache_ext.go)
itself.

### 2. xcaddy needs two `--with` flags for scopecache

xcaddy synthesises a top-level `go.mod` for the build. Go ignores
`replace` directives in dependency modules — so the `replace
github.com/VeloxCoding/scopecache => ../` inside `caddymodule/go.mod`
has no effect, and xcaddy fetches whatever the on-disk
`caddymodule/go.mod` pins (which may not match the local scopecache
source).

**Workaround:** pass `--with` for both root + caddymodule, pointing
each at the staged local source:

```
--with github.com/VeloxCoding/scopecache=/go/src/app/scopecache
--with github.com/VeloxCoding/scopecache/caddymodule=/go/src/app/scopecache/caddymodule
```

### 3. Absolute paths for `--with` (static build only)

In the static-builder-musl pipeline, xcaddy runs from
`.../buildroot/bin`. Relative paths like `./scopecache` resolve
against that directory and fail. Use absolute `/go/src/app/scopecache`
— which is where `build.sh` stages the source.

### 4. `FRANKENPHP_VERSION` must match the cloned tag

The default bake-file's in-container build script reads
`$FRANKENPHP_VERSION` and does `git checkout` on that ref. Default
is `"dev"` — but `build.sh` does a `--depth=1 --branch=vX.Y.Z` clone,
which only contains the tagged ref, not `dev`. Result: build fails
mid-pipeline trying to check out a missing ref.

**Workaround:** explicitly pass
`--set static-builder-musl.args.FRANKENPHP_VERSION=$FRANKENPHP_VERSION`
matching the cloned tag.

### 5. Platform must be pinned

`docker buildx bake` defaults to building both `linux/amd64` and
`linux/arm64` in parallel. On a single-architecture host (the typical
case) the cross-arch leg hangs forever waiting for binfmt
registration that isn't there.

**Workaround:** detect `uname -m`, set `BAKE_PLATFORM` to the host
arch, pass it as `--set static-builder-musl.platform=$BAKE_PLATFORM`.

### 6. `GITHUB_TOKEN` avoids mid-build rate-limits

The static-php-cli pipeline issues many anonymous github API calls
(downloading PHP, every extension's source, build deps). Without a
token, an anonymous IP hits the github API rate-limit somewhere
around minute 15 of a 30-minute build, and the build dies.

**Workaround:** export a `$GITHUB_TOKEN` env var before running.
A personal-access token with no special scopes is enough — it just
raises the rate-limit ceiling. The FrankenPHP `docker-bake.hcl`
declares `secret = ["id=github-token,env=GITHUB_TOKEN"]` on the
target, so bake reads the env var automatically — no CLI flag
needed (and `--secret` is not a valid bake CLI flag, even though
it is for `docker buildx build`). `build.sh` errors out early if
`GITHUB_TOKEN` is unset, so you can't accidentally start an hour-long
build that will die at minute 15.

### 7. xcaddy itself must be copied in separately

The FrankenPHP `1.12-builder-php8` image carries the Go toolchain
+ the FrankenPHP source, but **not** xcaddy. The static-builder bake
pipeline pulls xcaddy in via its own image-layer, so `build.sh`
itself doesn't need to handle this — but if you ever swap the static
pipeline for a plain `docker build` against
`dunglas/frankenphp:1.12-builder-php8` directly, you'll need:

```dockerfile
COPY --from=caddy:builder /usr/bin/xcaddy /usr/bin/xcaddy
```

### 8. WORK_DIR must be Docker-Desktop-bind-mountable

The pre-generation step (Stage 0) bind-mounts a temp directory into a
helper container that writes the generated C wrappers back to the
host. On Git-Bash / MSYS on Windows, `mktemp -d` returns paths under
`/tmp/tmp.XXX` — which is the **MSYS pseudo-FS**, not a real Windows
path Docker Desktop can bind-mount. The mount silently succeeds but
the writes never reach the host: the resulting "pre-generated"
extension source directory is **empty** on disk, the bake step
then COPYs that empty dir into the build context, and xcaddy fails
with `cannot find module providing package` because there's no
go.mod, no .go file, nothing.

**Workaround:** `WORK_DIR` is placed under `$SCRIPT_DIR/.build-work-$$`
(tools/frankenphp-bin/.build-work-PID), not in `/tmp`. That path is
under a real Windows drive (e:/, c:/) and Docker Desktop bind-mounts
it correctly. Cleanup trap removes the directory on exit.

If you change the pre-gen approach, keep this constraint in mind:
any `docker run -v HOSTPATH:CONTAINERPATH` step where the host
process expects to read the container's writes needs HOSTPATH to be
inside the project tree, not in `/tmp`.

### 9. FrankenPHP `1.12` pin

Earlier `:latest-builder` images shipped FrankenPHP source that
didn't match Caddy `v2.11.2` — xcaddy build fails with type-mismatch
errors in the Caddy glue layer. The `1.12-*` tags pin a working
combination.

If you bump `FRANKENPHP_VERSION`, also verify that the
`dunglas/frankenphp:<new>-builder-php8` image's Caddy glue is
compatible with the Caddy version xcaddy will pull. The compatibility
window is narrow — newer Caddy + older FrankenPHP glue (or vice
versa) breaks the build.

## Customising the embedded app

Edit [`embed/Caddyfile`](embed/Caddyfile) and
[`embed/public/index.php`](embed/public/index.php), then rerun
`build.sh`. The whole `embed/` tree gets baked into the binary's
internal filesystem at build time, so changes require a rebuild.

For runtime-only changes (no rebuild), serve from a real directory
and override at startup:

```bash
./frankenphp-static-linux-x86_64 run --config /etc/my.Caddyfile
```

…with a Caddyfile whose `root` points at a real directory on disk.

## Where the binary goes after building

`build.sh` writes to [`examples/frankenphp-bin/`](../../examples/frankenphp-bin/)
in the repo root. See that directory's README for run instructions.
