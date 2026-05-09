#!/usr/bin/env bash
# install_caddyscope.sh — fresh-VPS install of Caddy + scopecache module.
#
# Performs the full install in one shot, idempotently:
#   0. apt update + apt upgrade
#   1. Download caddyscope binary + SHA256SUMS from GitHub Releases,
#      verify checksum, move into /usr/local/bin/caddy
#   2. Create caddy:caddy system user/group (skip if already present)
#   3. Write /etc/caddy/Caddyfile (back up any existing one first)
#   4. Write /etc/systemd/system/caddy.service, reload + enable + start
#   5. Smoke-test http://localhost:<PORT>/help
#   6. apt install -y wrk (so install_and_benchmark.sh can run later)
#
# Usage:
#   sudo ./install_caddyscope.sh
#
# Configurable via env vars (defaults shown):
#
#   VERSION=latest          release tag to install (e.g. v0.8.18). The
#                           literal string "latest" auto-resolves to
#                           the most recent release tag on GitHub.
#   PORT=80                 TCP port Caddy listens on
#   SCOPE_MAX_ITEMS=100000  per-scope item cap
#   MAX_STORE_MB=100        store-wide byte cap
#   MAX_ITEM_MB=1           per-item byte cap
#
# Requires: Ubuntu / Debian, root (or run via sudo). Architecture is
# auto-detected (x86_64 -> amd64, aarch64 -> arm64).
#
# After this script finishes the cache is reachable on
# http://<vps-ip>:<PORT>/. To validate end-to-end with a real
# workload, fetch and run scripts/install_and_benchmark.sh from the
# same repo.

set -euo pipefail

VERSION="${VERSION:-latest}"
PORT="${PORT:-80}"
SCOPE_MAX_ITEMS="${SCOPE_MAX_ITEMS:-100000}"
MAX_STORE_MB="${MAX_STORE_MB:-100}"
MAX_ITEM_MB="${MAX_ITEM_MB:-1}"

REPO_OWNER="VeloxCoding"
REPO_NAME="scopecache"

# --- preflight -----------------------------------------------------

if [ "$(id -u)" -ne 0 ]; then
    echo "this script must run as root (try: sudo $0)" >&2
    exit 1
fi

ARCH=$(uname -m)
case "$ARCH" in
    x86_64)  GO_ARCH=amd64 ;;
    aarch64) GO_ARCH=arm64 ;;
    *)
        echo "unsupported architecture: $ARCH (only x86_64 and aarch64 are supported)" >&2
        exit 1
        ;;
esac

if ! command -v curl >/dev/null 2>&1; then
    echo "curl is required and not installed; install it first (apt install -y curl)" >&2
    exit 1
fi

# --- step 0: apt update + upgrade ----------------------------------

echo "[0/6] apt update + apt upgrade"
apt-get update -qq
DEBIAN_FRONTEND=noninteractive apt-get -y -qq -o Dpkg::Options::=--force-confdef \
    -o Dpkg::Options::=--force-confold upgrade

# --- step 1: download + verify binary ------------------------------

if [ "$VERSION" = "latest" ]; then
    # GitHub redirects /releases/latest to the newest tag URL; the tag
    # is the trailing path component. -o /dev/null + -w %{redirect_url}
    # avoids downloading the page body.
    VERSION=$(curl -fsSL -o /dev/null -w '%{redirect_url}' \
        "https://github.com/${REPO_OWNER}/${REPO_NAME}/releases/latest" \
        | sed 's|.*/tag/||')
    if [ -z "$VERSION" ]; then
        echo "could not auto-detect the latest release tag" >&2
        exit 1
    fi
    echo "[1/6] downloading caddyscope-linux-${GO_ARCH} (resolved latest = ${VERSION})"
else
    echo "[1/6] downloading caddyscope-linux-${GO_ARCH} (pinned ${VERSION})"
fi

DOWNLOAD_DIR=$(mktemp -d)
trap 'rm -rf "$DOWNLOAD_DIR"' EXIT

BINARY="caddyscope-linux-${GO_ARCH}"
URL_BASE="https://github.com/${REPO_OWNER}/${REPO_NAME}/releases/download/${VERSION}"

curl -fsSL -o "${DOWNLOAD_DIR}/${BINARY}"  "${URL_BASE}/${BINARY}"
curl -fsSL -o "${DOWNLOAD_DIR}/SHA256SUMS" "${URL_BASE}/SHA256SUMS"

(
    cd "$DOWNLOAD_DIR"
    sha256sum --check --ignore-missing SHA256SUMS
)

# --- step 2: install binary ----------------------------------------

echo "[2/6] installing /usr/local/bin/caddy"
install -m 0755 "${DOWNLOAD_DIR}/${BINARY}" /usr/local/bin/caddy
/usr/local/bin/caddy version

# --- step 3: caddy user/group --------------------------------------

echo "[3/6] ensuring caddy:caddy system user/group exists"
if ! getent group caddy >/dev/null; then
    groupadd --system caddy
fi
if ! getent passwd caddy >/dev/null; then
    useradd --system \
        --gid caddy \
        --create-home \
        --home-dir /var/lib/caddy \
        --shell /usr/sbin/nologin \
        --comment "Caddy web server" \
        caddy
fi

# --- step 4: Caddyfile ---------------------------------------------

echo "[4/6] writing /etc/caddy/Caddyfile"
mkdir -p /etc/caddy
if [ -f /etc/caddy/Caddyfile ]; then
    backup="/etc/caddy/Caddyfile.bak.$(date +%Y%m%d-%H%M%S)"
    cp /etc/caddy/Caddyfile "$backup"
    echo "  existing Caddyfile backed up to ${backup}"
fi
tee /etc/caddy/Caddyfile > /dev/null <<EOF
{
    admin off
}

:${PORT} {
    scopecache {
        scope_max_items ${SCOPE_MAX_ITEMS}
        max_store_mb    ${MAX_STORE_MB}
        max_item_mb     ${MAX_ITEM_MB}
    }
    respond 404
}
EOF

# --- step 5: systemd unit + start ----------------------------------

echo "[5/6] writing systemd unit + starting caddy"
tee /etc/systemd/system/caddy.service > /dev/null <<'EOF'
[Unit]
Description=Caddy with scopecache module
Documentation=https://caddyserver.com/docs/
After=network.target network-online.target
Requires=network-online.target

[Service]
Type=notify
User=caddy
Group=caddy
ExecStart=/usr/local/bin/caddy run --environ --config /etc/caddy/Caddyfile
ExecReload=/usr/local/bin/caddy reload --config /etc/caddy/Caddyfile --force
TimeoutStopSec=5s
LimitNOFILE=1048576
PrivateTmp=true
ProtectSystem=full
AmbientCapabilities=CAP_NET_BIND_SERVICE
Restart=on-failure
RestartSec=5s

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable --now caddy

# Type=notify means systemctl returns once Caddy reports ready, but
# add a tiny retry loop on /help to also confirm the HTTP listener
# answered at least once before claiming success.
echo "  smoke-testing http://localhost:${PORT}/help"
ok=0
for _ in 1 2 3 4 5; do
    if curl -fsS --max-time 2 "http://localhost:${PORT}/help" >/dev/null; then
        ok=1
        break
    fi
    sleep 1
done
if [ "$ok" -ne 1 ]; then
    echo "smoke-test FAILED — see 'systemctl status caddy --no-pager' and 'journalctl -u caddy -n 50'" >&2
    exit 1
fi

# --- step 6: install wrk -------------------------------------------

echo "[6/6] installing wrk (benchmark tool)"
DEBIAN_FRONTEND=noninteractive apt-get install -y -qq wrk

# --- done ----------------------------------------------------------

echo
echo "Done. Caddy + scopecache (${VERSION}) is running on :${PORT}."
echo
echo "Try it:"
echo "  curl http://localhost:${PORT}/help"
echo "  curl -X POST http://localhost:${PORT}/append \\"
echo "    -H 'Content-Type: application/json' \\"
echo "    -d '{\"scope\":\"demo\",\"payload\":{\"msg\":\"hello\"}}'"
echo "  curl 'http://localhost:${PORT}/tail?scope=demo'"
echo
echo "Benchmark it:"
echo "  wget https://raw.githubusercontent.com/${REPO_OWNER}/${REPO_NAME}/main/scripts/install_and_benchmark.sh"
echo "  chmod +x install_and_benchmark.sh"
echo "  ./install_and_benchmark.sh"
echo
echo "Service control:"
echo "  systemctl status caddy --no-pager"
echo "  systemctl reload caddy           # after editing /etc/caddy/Caddyfile"
echo "  journalctl -u caddy -f           # live logs"
