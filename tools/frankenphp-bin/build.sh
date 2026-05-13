#!/usr/bin/env bash
# build.sh — produce a fully-static FrankenPHP + scopecache binary
# with all addons compiled in:
#
#   - scopecache (root + caddymodule)
#   - addons/guarded         (auto-pulled via caddymodule/module.go)
#   - addons/frankenphp-ext  (PHP extension, exposes scopecache_* to PHP)
#
# Output: examples/frankenphp-bin/frankenphp-static-linux-<arch>
# musl-linked, runs on any modern x86_64 / arm64 Linux without
# external dependencies.
#
# Time : ~15-45 min cold (compiles PHP + every extension from source).
# Disk : build pulls ~5-10 GB of intermediate docker layers.
#
# Usage:
#   ./build.sh
#   FRANKENPHP_VERSION=v1.12.2 ./build.sh
#   PHP_EXTENSIONS=curl,opcache ./build.sh
#   GITHUB_TOKEN=ghp_... ./build.sh       # avoids github API rate limits
#
# Build-chain pitfalls (full rationale in README.md):
#   - PHP-extension wrappers must be pre-generated (static-builder-musl
#     does not know about //export_php: directives).
#   - Two scopecache --with flags (Go ignores `replace` directives in deps).
#   - Absolute --with paths (xcaddy runs from buildroot/bin in static mode).
#   - FRANKENPHP_VERSION must match the cloned tag.
#   - Platform must be pinned (bake defaults to amd64+arm64 parallel).
#   - GITHUB_TOKEN advised to avoid rate-limits mid-build.

set -euo pipefail

FRANKENPHP_VERSION="${FRANKENPHP_VERSION:-v1.12.2}"
PHP_EXTENSIONS="${PHP_EXTENSIONS:-curl,opcache,openssl,mbstring,sodium,pdo,pdo_sqlite,session,tokenizer,filter,ctype,iconv}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
DIST_DIR="$REPO_ROOT/examples/frankenphp-bin"

EXT_BUILDER_IMAGE="frankenphp-ext-builder:latest"

# WORK_DIR under SCRIPT_DIR instead of mktemp's default /tmp/tmp.XXX —
# Git-Bash on Windows places /tmp under MSYS's pseudo-FS which Docker
# Desktop cannot bind-mount. Files written via -v ... :/out in the
# pre-gen container silently don't reach the host. Putting WORK_DIR
# under the project tree avoids that. Cleanup trap below removes it.
WORK_DIR="$SCRIPT_DIR/.build-work-$$"
trap 'rm -rf "$WORK_DIR"' EXIT
mkdir -p "$WORK_DIR"

mkdir -p "$DIST_DIR"

# ---- Stage 0: pre-generate the PHP-extension C wrappers --------------------
# static-builder-musl runs xcaddy directly against the source we --with;
# it does not run frankenphp-gen extension-init. So we pre-generate the
# C wrappers in the same gen-image tools/frankenphp-ext uses, then stage
# the generated source as the addon's --with target.

if ! docker image inspect "$EXT_BUILDER_IMAGE" >/dev/null 2>&1; then
    echo ">>> Building $EXT_BUILDER_IMAGE (one-time, ~3 min)..."
    ( cd "$REPO_ROOT/tools/frankenphp-ext" && \
      MSYS_NO_PATHCONV=1 docker build -t "$EXT_BUILDER_IMAGE" -f Dockerfile.gen . )
fi

EXT_GEN_DIR="$WORK_DIR/ext-prebuilt"
mkdir -p "$EXT_GEN_DIR"

echo ">>> Pre-generating PHP-extension C wrappers via $EXT_BUILDER_IMAGE..."
MSYS_NO_PATHCONV=1 docker run --rm \
    -v "$REPO_ROOT/addons/frankenphp-ext:/ext-src:ro" \
    -v "$EXT_GEN_DIR:/out" \
    "$EXT_BUILDER_IMAGE" \
    bash -c '
        set -euo pipefail
        mkdir -p /work/ext
        cp /ext-src/go.mod /work/ext/
        cp /ext-src/scopecache_ext.go /work/ext/

        # Fix gofmt-rewritten directive (README.md pitfall #1 of frankenphp-ext).
        sed -i "s|^// export_php:|//export_php:|g" /work/ext/scopecache_ext.go

        cd /work/ext
        /usr/local/bin/frankenphp-gen extension-init scopecache_ext.go

        # Patch nullable returns (README.md pitfall #2 of frankenphp-ext).
        for f in /work/ext/build/*.c /work/ext/*.c; do
            [ -f "$f" ] || continue
            sed -i -e "s|RETURN_EMPTY_STRING();|RETURN_NULL();|g" \
                   -e "s|RETURN_EMPTY_ARRAY();|RETURN_NULL();|g" "$f"
        done

        # Rewrite the replace directive: the static build will see the
        # scopecache root at /go/src/app/scopecache.
        sed -i "s|^// replace github.com/VeloxCoding/scopecache => ../\\.\\.|replace github.com/VeloxCoding/scopecache => /go/src/app/scopecache|" /work/ext/go.mod

        cp -r /work/ext/. /out/
    '

# ---- Stage 1: clone FrankenPHP + stage scopecache + extension source -------

echo ">>> Cloning php/frankenphp@$FRANKENPHP_VERSION..."
git clone --depth=1 --branch "$FRANKENPHP_VERSION" \
    https://github.com/php/frankenphp "$WORK_DIR/fp" 2>&1 | tail -3

echo ">>> Copying scopecache source into build context (lands at /go/src/app/scopecache)..."
SCOPECACHE_DIR="$WORK_DIR/fp/scopecache"
mkdir -p "$SCOPECACHE_DIR"
cp "$REPO_ROOT/go.mod" "$SCOPECACHE_DIR/"
cp "$REPO_ROOT"/*.go "$SCOPECACHE_DIR/"
cp -r "$REPO_ROOT/cmd" "$SCOPECACHE_DIR/"
cp -r "$REPO_ROOT/caddymodule" "$SCOPECACHE_DIR/"
cp -r "$REPO_ROOT/addons" "$SCOPECACHE_DIR/"

# Stage the pre-generated extension SIBLING to scopecache, not INSIDE
# its tree. xcaddy's module resolver gets confused when one --with
# path is a subdirectory of another (both have their own go.mod, but
# xcaddy's path-handling collides). Mirrors the dynamic build, which
# uses /work/scopecache + /work/ext side-by-side.
echo ">>> Staging pre-generated extension as sibling to scopecache..."
EXT_STAGED_DIR="$WORK_DIR/fp/scopecache-ext"
cp -r "$EXT_GEN_DIR" "$EXT_STAGED_DIR"
# Remove the conflicting copy that came in via cp -r addons.
rm -rf "$SCOPECACHE_DIR/addons/frankenphp-ext"

echo ">>> Copying embed/ (Caddyfile + PHP app) into build context..."
cp -r "$SCRIPT_DIR/embed" "$WORK_DIR/fp/embed"

cd "$WORK_DIR/fp"

# Absolute --with paths: xcaddy runs from .../buildroot/bin during the
# static build, so relative paths like ./scopecache do not resolve.
# Three --with flags: root (Go ignores replace in deps), caddymodule,
# and the PHP-extension addon.
XCADDY_ARGS="--with github.com/VeloxCoding/scopecache=/go/src/app/scopecache --with github.com/VeloxCoding/scopecache/caddymodule=/go/src/app/scopecache/caddymodule --with github.com/VeloxCoding/scopecache/addons/frankenphp-ext=/go/src/app/scopecache-ext"

if [ -z "${GITHUB_TOKEN:-}" ]; then
    echo "ERROR: GITHUB_TOKEN is required. The bake pipeline issues many" >&2
    echo "       github API calls (spc downloads PHP source + extensions);" >&2
    echo "       anonymous IPs hit 60-req/h rate-limit and the build dies" >&2
    echo "       mid-way. Generate a PAT at github.com/settings/tokens (no" >&2
    echo "       special scopes needed), then:" >&2
    echo "         export GITHUB_TOKEN=ghp_xxxxxxxxxxx" >&2
    echo "         ./build.sh" >&2
    exit 1
fi

echo ">>> Running docker buildx bake static-builder-musl (slow part — grab koffie)..."
HOST_ARCH=$(uname -m)
case "$HOST_ARCH" in
    x86_64)        BAKE_PLATFORM="linux/amd64" ;;
    aarch64|arm64) BAKE_PLATFORM="linux/arm64" ;;
    *) echo "Unsupported arch: $HOST_ARCH"; exit 1 ;;
esac

# The FrankenPHP docker-bake.hcl declares
# `secret = ["id=github-token,env=GITHUB_TOKEN"]` on this target,
# so just exporting GITHUB_TOKEN is enough — bake reads it from the
# environment. `--secret` is a docker-buildx-BUILD flag, not bake.
docker buildx bake --load \
    --set "static-builder-musl.platform=$BAKE_PLATFORM" \
    --set "static-builder-musl.args.FRANKENPHP_VERSION=$FRANKENPHP_VERSION" \
    --set "static-builder-musl.args.XCADDY_ARGS=$XCADDY_ARGS" \
    --set "static-builder-musl.args.PHP_EXTENSIONS=$PHP_EXTENSIONS" \
    --set "static-builder-musl.args.EMBED=./embed" \
    static-builder-musl

echo ">>> Build finished — extracting binary from image..."
ARCH=$(uname -m)
CONTAINER_ID=$(docker create dunglas/frankenphp:static-builder-musl)
OUT="$DIST_DIR/frankenphp-static-linux-$ARCH"
docker cp "$CONTAINER_ID:/go/src/app/dist/frankenphp-linux-$ARCH" "$OUT"
docker rm "$CONTAINER_ID" >/dev/null
chmod +x "$OUT"

echo ""
echo ">>> Done."
ls -lh "$OUT"
echo ""
echo "    Linked modules:"
file "$OUT" | sed 's/^/    /'
echo ""
echo "    Run instructions: examples/frankenphp-bin/README.md"
