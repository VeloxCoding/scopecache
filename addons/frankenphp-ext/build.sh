#!/usr/bin/env bash
# build.sh — produce ./dist/frankenphp with the scopecache extension
# compiled in. Two-stage flow:
#
#   1. Build (or reuse) the cached generator image defined in
#      Dockerfile.gen — that image carries frankenphp-gen + xcaddy +
#      gen_stub.php + the PHP-ZTS headers. ~3 min first time, then
#      docker's layer cache makes subsequent runs instant. If you
#      want a clean rebuild of the image: ./build.sh --rebuild-gen-image
#
#   2. Run that image once with the scopecache source + this extension
#      source bind-mounted, generate the cgo wrappers, and `xcaddy
#      build` the final binary into /out → ./dist/frankenphp on the
#      host. ~1-3 min warm.
#
# Cold-cold (no cached image, no docker layer cache): ~10-15 min.
# Warm (image cached, code edited):                   ~1-3 min.
#
# See CLAUDE_PHPEXTENSION_IN_GO.md in the repo root for the catalog
# of build-chain pitfalls each step exists to dodge.
#
# Usage:
#   ./build.sh
#   ./build.sh --rebuild-gen-image     # force-rebuild the cached image

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
DIST_DIR="$SCRIPT_DIR/dist"
GEN_IMAGE="scopecache-ext-builder:latest"

mkdir -p "$DIST_DIR"

# ---- Stage 1: cached generator image -----------------------------------------

DOCKER_BUILD_ARGS=()
if [ "${1:-}" = "--rebuild-gen-image" ]; then
    echo ">>> [pre] --rebuild-gen-image: forcing a clean rebuild of $GEN_IMAGE"
    DOCKER_BUILD_ARGS+=(--no-cache)
fi

echo ">>> [pre] building (or reusing cached) $GEN_IMAGE"
# cd + "." as context: avoids Git-Bash on Windows rewriting an absolute
# /e/... path that the Windows docker daemon can't resolve.
( cd "$SCRIPT_DIR" && MSYS_NO_PATHCONV=1 docker build \
    "${DOCKER_BUILD_ARGS[@]}" \
    -t "$GEN_IMAGE" \
    -f Dockerfile.gen \
    . )

# ---- Stage 2: extension build inside the cached image ------------------------
#
# MSYS_NO_PATHCONV=1 stops Git-Bash on Windows from rewriting
# in-container paths (/work, /scopecache, /ext-src, /out) into
# Windows absolute paths before docker sees them.
MSYS_NO_PATHCONV=1 docker run --rm \
    -v "$REPO_ROOT:/scopecache:ro" \
    -v "$SCRIPT_DIR:/ext-src:ro" \
    -v "$DIST_DIR:/out" \
    -w /work \
    "$GEN_IMAGE" \
    bash -c '
        set -euo pipefail

        echo ">>> [1/3] staging scopecache + extension source under /work"
        # Stage scopecache root + caddymodule + addons (caddymodule
        # imports addons/guarded — see caddymodule/module.go:28 — so
        # the addons tree must be present for the xcaddy build to
        # resolve that import). cmd/ is skipped because xcaddy does
        # not --with the standalone-binary package and nothing on
        # the build path imports it.
        #
        # Note: copying all of /scopecache/addons re-stages the
        # extension source under /work/scopecache/addons/frankenphp-ext.
        # That copy is shadowed by the xcaddy --with that points the
        # extension module at /work/ext instead, so the duplicate is
        # harmless. Could be excluded for tidiness; not worth the
        # extra build-script logic.
        #
        # NB no apostrophes in these comments — they would close the
        # outer bash -c "..." single-quoted string. See pitfall #8 in
        # CLAUDE_PHPEXTENSION_IN_GO.md.
        mkdir -p /work/scopecache /work/ext
        cp /scopecache/go.mod /work/scopecache/
        cp /scopecache/*.go /work/scopecache/
        cp -r /scopecache/caddymodule /work/scopecache/caddymodule
        cp -r /scopecache/addons /work/scopecache/addons
        cp /ext-src/go.mod /work/ext/
        cp /ext-src/scopecache_ext.go /work/ext/

        # Activate the replace directive so the extension builds against
        # the staged scopecache source, not the published release.
        sed -i "s|^// replace github.com/VeloxCoding/scopecache => ../\\.\\.|replace github.com/VeloxCoding/scopecache => /work/scopecache|" /work/ext/go.mod

        # Undo the gofmt-rewritten directive on the staged copy. Source
        # on disk has "// export_php:" (with space) so gofmt and the
        # pre-commit hook leave it alone; the generator only matches
        # the tight "//export_php:" form. See pitfall #6(b) in
        # CLAUDE_PHPEXTENSION_IN_GO.md for the full rationale.
        sed -i "s|^// export_php:|//export_php:|g" /work/ext/scopecache_ext.go

        echo ">>> [2/3] frankenphp-gen extension-init"
        cd /work/ext
        /usr/local/bin/frankenphp-gen extension-init scopecache_ext.go

        # Patch the generated C wrappers: the upstream extgen template
        # emits RETURN_EMPTY_STRING() / RETURN_EMPTY_ARRAY() when our Go
        # function returns nil, regardless of whether the directive
        # declared the return as `?string` / `?array` (nullable). That
        # collapses PHP NULL into "" / [], breaking the idiomatic
        # `if ($r === null)` miss check. Patch back to RETURN_NULL().
        # See pitfall #10 in CLAUDE_PHPEXTENSION_IN_GO.md.
        echo "  patching generated C wrappers for ?string/?array NULL semantics"
        for f in /work/ext/build/*.c /work/ext/*.c; do
            [ -f "$f" ] || continue
            sed -i -e "s|RETURN_EMPTY_STRING();|RETURN_NULL();|g" \
                   -e "s|RETURN_EMPTY_ARRAY();|RETURN_NULL();|g" "$f"
        done

        echo ">>> [3/3] xcaddy build"
        # CGO build flags:
        #   -D_GNU_SOURCE      = PHP-Zend headers reference memrchr() (GNU ext).
        #   -linkmode=external = Go 1.26 internal linker chokes when stitching
        #                        multiple cgo packages (frankenphp + our ext).
        CGO_ENABLED=1 \
        XCADDY_GO_BUILD_FLAGS="-ldflags=-linkmode=external" \
        CGO_CFLAGS="-D_GNU_SOURCE $(php-config --includes)" \
        CGO_LDFLAGS="$(php-config --ldflags) $(php-config --libs)" \
        xcaddy build \
            --output /out/frankenphp \
            --with github.com/dunglas/frankenphp=/go/src/app \
            --with github.com/dunglas/frankenphp/caddy=/go/src/app/caddy \
            --with github.com/VeloxCoding/scopecache=/work/scopecache \
            --with github.com/VeloxCoding/scopecache/caddymodule=/work/scopecache/caddymodule \
            --with github.com/VeloxCoding/scopecache/addons/frankenphp-ext=/work/ext

        ls -lh /out/frankenphp
    '

echo
echo "Binary: $DIST_DIR/frankenphp"
echo
echo "Validate: $SCRIPT_DIR/smoke.sh"
echo "Bench:    $SCRIPT_DIR/bench.php (see Dockerfile.bench for the runtime)"
