#!/usr/bin/env bash
# build.sh — compile a FrankenPHP binary that includes the scopecache
# PHP extension. Outputs ./dist/frankenphp.
#
# Why this is more involved than the obvious "use the builder image":
# the FrankenPHP `extension-init` subcommand we need to generate the
# cgo/PHP boilerplate landed in master after the 1.12 release. As of
# this writing the published `dunglas/frankenphp:*-builder-*` images
# do not yet include it. So step 1 below rebuilds the `frankenphp`
# binary from master inside the builder image to get the generator.
#
# What this does, end-to-end:
#
#   1. Spins up the 1.12-builder image (PHP-ZTS headers + Go toolchain).
#   2. Inside the container:
#      a. git clone php/frankenphp@main, build caddy/frankenphp/main.go
#         to /usr/local/bin/frankenphp-gen — a binary that exposes
#         `frankenphp-gen extension-init` (EXPERIMENTAL subcommand).
#      b. Stage the scopecache source + this extension source into the
#         workdir so `replace` directives resolve cleanly.
#      c. Run `frankenphp-gen extension-init scopecache_ext.go` — this
#         generates the cgo wrappers (scopecache_ext_generated.go),
#         the PHP-side stub (scopecache_ext.stub.php), and a build/
#         subdirectory that exposes a Caddy module xcaddy can --with.
#      d. Run xcaddy build with all four --with flags (frankenphp,
#         frankenphp/caddy, scopecache core, scopecache caddymodule,
#         and the generated build/ directory), producing a FrankenPHP
#         binary that exposes scopecache_get() to PHP.
#   3. Copies the resulting binary to ./dist/frankenphp.
#
# Cold build: ~10-20 min (clone + master frankenphp build + xcaddy).
# Warm rebuild: ~1-3 min if the docker cache is intact.
#
# Usage:
#   ./build.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
DIST_DIR="$SCRIPT_DIR/dist"

FRANKENPHP_VERSION="${FRANKENPHP_VERSION:-1.12}"
BUILDER_IMAGE="dunglas/frankenphp:${FRANKENPHP_VERSION}-builder-php8"

mkdir -p "$DIST_DIR"

# xcaddy itself contains no cgo, but the Go 1.26.x toolchain in the
# FrankenPHP 1.12 builder image has a known regression that surfaces
# `runtime/cgo: relocation target stderr not defined` on every
# `go install`. The harness Dockerfile sidesteps this by pulling the
# xcaddy binary out of the caddy:builder image — same trick here.
XCADDY_HOST_BIN="$DIST_DIR/.xcaddy"
if [ ! -x "$XCADDY_HOST_BIN" ]; then
    echo ">>> [pre] extracting xcaddy from caddy:builder image..."
    # docker cp on Windows Git-Bash mangles host paths, so stream
    # the binary out through docker run + stdout instead.
    MSYS_NO_PATHCONV=1 docker run --rm --entrypoint cat caddy:builder \
        /usr/bin/xcaddy > "$XCADDY_HOST_BIN"
    chmod +x "$XCADDY_HOST_BIN"
fi

# MSYS_NO_PATHCONV=1 prevents Git-Bash on Windows from rewriting the
# in-container paths (e.g. "/work") into Windows absolute paths.
MSYS_NO_PATHCONV=1 docker run --rm \
    -v "$REPO_ROOT:/scopecache:ro" \
    -v "$SCRIPT_DIR:/ext-src:ro" \
    -v "$DIST_DIR:/out" \
    -v "$XCADDY_HOST_BIN:/usr/local/bin/xcaddy:ro" \
    -w /work \
    "$BUILDER_IMAGE" \
    bash -c '
        set -euo pipefail

        echo ">>> [1/4] cloning frankenphp@main and building the generator binary"
        git clone --depth=1 https://github.com/php/frankenphp /work/fp-master 2>&1 | tail -1
        cd /work/fp-master/caddy/frankenphp
        CGO_CFLAGS="$(php-config --includes)" \
        CGO_LDFLAGS="$(php-config --ldflags) $(php-config --libs)" \
            go build -o /usr/local/bin/frankenphp-gen . 2>&1 | tail -5
        /usr/local/bin/frankenphp-gen --help 2>&1 | grep -E "extension-init" >/dev/null || {
            echo "ERROR: built frankenphp does not have extension-init subcommand" >&2
            exit 1
        }
        echo "  ok — frankenphp-gen has extension-init"

        echo ">>> [2/4] staging scopecache + extension source under /work"
        mkdir -p /work/scopecache /work/ext
        cp /scopecache/go.mod /work/scopecache/
        cp /scopecache/*.go /work/scopecache/
        cp -r /scopecache/cmd /work/scopecache/cmd
        cp -r /scopecache/caddymodule /work/scopecache/caddymodule
        cp -r /scopecache/addons /work/scopecache/addons
        cp /ext-src/go.mod /work/ext/
        cp /ext-src/scopecache_ext.go /work/ext/

        # Activate the replace directive so the extension builds against
        # the staged scopecache source, not the published v0.8.22 release.
        sed -i "s|^// replace github.com/VeloxCoding/scopecache => ../\\.\\.|replace github.com/VeloxCoding/scopecache => /work/scopecache|" /work/ext/go.mod

        # Restore the tight //export_php: directive form that the
        # frankenphp-gen parser requires. The source file on disk has
        # "// export_php:" (with a space) so it stays gofmt-compliant
        # and the project pre-commit hook accepts it. gofmt does not
        # recognise //export_php: as a directive (it is not on the Go
        # hardcoded whitelist alongside //go:, //line, //export, etc.)
        # and would otherwise normalise away the very form the
        # generator needs. This sed un-normalises the staged copy
        # immediately before extension-init runs.
        sed -i "s|^// export_php:|//export_php:|g" /work/ext/scopecache_ext.go

        echo ">>> [3/4] running frankenphp-gen extension-init"
        cd /work/ext
        # gen_stub.php lives under /usr/local/lib/php/build in the
        # 1.12-builder image; the generator defaults to looking under
        # /usr/local/src/php/build, so point it at the real location.
        GEN_STUB_SCRIPT="$(find /usr/local -name gen_stub.php -path '*/build/*' 2>/dev/null | head -1)"
        if [ -z "$GEN_STUB_SCRIPT" ]; then
            echo "ERROR: gen_stub.php not found in the builder image" >&2
            exit 1
        fi
        echo "  gen_stub.php: $GEN_STUB_SCRIPT"
        export GEN_STUB_SCRIPT
        /usr/local/bin/frankenphp-gen extension-init scopecache_ext.go
        echo "  generated files:"
        ls -la /work/ext/build/ 2>/dev/null || ls -la /work/ext/

        echo ">>> [4/4] xcaddy build (frankenphp@main + scopecache + extension)"
        # xcaddy is bind-mounted from the caddy:builder image by the
        # outer shell — see the XCADDY_HOST_BIN handling above. The
        # 1.12-builder Go toolchain cannot `go install` it because
        # of an unrelated runtime/cgo regression that fires on any
        # go install in this image.

        cd /work/ext
        # Use the FrankenPHP source bundled in the builder image at
        # /go/src/app, which is pinned to the image PHP-ZTS headers,
        # for the host build. frankenphp-gen from master only
        # generated the wrappers; the frankenphp Go API the wrappers
        # use - RegisterExtension - has been stable since 1.6.
        # The generator wrote the cgo wrappers in-place, so the
        # extension --with path is /work/ext itself.
        #
        # -linkmode=external forces the system linker. The Go 1.26
        # internal linker bailed with "runtime/cgo: relocation target
        # stderr not defined" when stitching multiple cgo packages
        # (frankenphp + our extension), even though both link against
        # the same libc that defines stderr.
        # -D_GNU_SOURCE is required for the GNU-extension memrchr()
        # that PHP-Zend headers reference. Without it, the cgo compile
        # step bails with "implicit declaration of function memrchr".
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

        echo ">>> done"
        ls -lh /out/frankenphp
    '

echo
echo "Binary: $DIST_DIR/frankenphp"
echo
echo "Try it:"
echo "  cd $SCRIPT_DIR"
echo "  $DIST_DIR/frankenphp php-server -listen :8080 -root ."
echo "  curl http://localhost:8080/test.php"
