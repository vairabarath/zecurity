#!/usr/bin/env bash
# client-local-install.sh — ZECURITY client local installer
#
# Installs:
#   - /usr/local/bin/zecurity-client              (from local binary)
#   - /etc/zecurity/client.conf                    (config, mode 0644)
#   - /etc/systemd/system/zecurity-client.service  (User= set to enrolling user)
#
# The daemon socket (/run/zecurity-client/daemon.sock) is created at service
# start via RuntimeDirectory=zecurity-client in the service file — no action
# needed here.
#
# The encrypted state directory (~/.local/share/zecurity-client/) is created
# by the daemon on first login — no action needed here.
#
# Required: run with sudo so the binary and service file can be installed.
# SUDO_USER must be set (automatically set when using sudo).
#
# Optional environment variables (dev overrides — omit to use compiled defaults):
#   CONTROLLER_ADDR       — gRPC address, e.g. "localhost:9090"
#   CONTROLLER_HTTP_ADDR  — HTTP address, e.g. "localhost:8080"
#
# Usage:
#   sudo ./scripts/client-local-install.sh <path-to-binary>
#   sudo CONTROLLER_ADDR=localhost:9090 CONTROLLER_HTTP_ADDR=localhost:8080 \
#     ./scripts/client-local-install.sh <path-to-binary>

set -euo pipefail

# ── Defaults ────────────────────────────────────────────────────────────────
INSTALL_BIN="/usr/local/bin/zecurity-client"
CONFIG_DIR="/etc/zecurity"
CONFIG_FILE="${CONFIG_DIR}/client.conf"
SYSTEMD_DIR="/etc/systemd/system"
SERVICE_NAME="zecurity-client.service"

# ── Helpers ─────────────────────────────────────────────────────────────────
log()  { printf '\033[1;34m[install]\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[install]\033[0m %s\n' "$*" >&2; }
err()  { printf '\033[1;31m[install]\033[0m %s\n' "$*" >&2; exit 1; }

usage() {
    cat <<EOF
Usage: sudo $0 <path-to-binary>

Installs the ZECURITY client locally.

Optional env vars (dev overrides — omit to use compiled defaults):
  CONTROLLER_ADDR       gRPC host:port   (e.g. localhost:9090)
  CONTROLLER_HTTP_ADDR  HTTP host:port   (e.g. localhost:8080)

After install, run as the enrolling user:
  zecurity-client setup
  zecurity-client login
EOF
}

# ── Checks ──────────────────────────────────────────────────────────────────
[[ $EUID -eq 0 ]] || err "must run as root (use sudo)"

if [[ $# -ne 1 ]]; then
    usage
    exit 1
fi

LOCAL_BINARY="$1"
[[ -f "$LOCAL_BINARY" ]] || err "binary not found at $LOCAL_BINARY"

# Determine the enrolling user — the real user who invoked sudo.
ENROLLING_USER="${SUDO_USER:-}"
[[ -n "$ENROLLING_USER" ]] || err "SUDO_USER is not set — run with sudo, not as root directly"
id "$ENROLLING_USER" >/dev/null 2>&1 || err "user '$ENROLLING_USER' does not exist on this system"

command -v systemctl >/dev/null || err "systemctl is required"

log "installing for user: $ENROLLING_USER"

# ── Find repo root ───────────────────────────────────────────────────────────
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
SERVICE_SRC="${REPO_ROOT}/client/zecurity-client.service"
[[ -f "$SERVICE_SRC" ]] || err "service file not found at $SERVICE_SRC"

# ── Install binary ──────────────────────────────────────────────────────────
log "installing binary to $INSTALL_BIN"
install -m 0755 -o root -g root "$LOCAL_BINARY" "$INSTALL_BIN"

# ── Install systemd service file (with User= substituted) ───────────────────
log "installing $SERVICE_NAME (User=$ENROLLING_USER)"
sed "s/^User=$/User=${ENROLLING_USER}/" "$SERVICE_SRC" \
    > "${SYSTEMD_DIR}/${SERVICE_NAME}"
chmod 0644 "${SYSTEMD_DIR}/${SERVICE_NAME}"

# ── Write config file ────────────────────────────────────────────────────────
# /etc/zecurity/ may already exist if the connector is installed on the same host.
log "creating $CONFIG_DIR"
install -d -m 0755 "$CONFIG_DIR"

log "writing $CONFIG_FILE"
{
    printf '# ZECURITY client configuration — written by client-local-install.sh\n'
    printf '# workspace is set by: zecurity-client setup\n'
    printf 'workspace = ""\n'
    if [[ -n "${CONTROLLER_ADDR:-}" ]]; then
        printf 'controller_address = "%s"\n' "$CONTROLLER_ADDR"
    fi
    if [[ -n "${CONTROLLER_HTTP_ADDR:-}" ]]; then
        printf 'http_base_url = "http://%s"\n' "$CONTROLLER_HTTP_ADDR"
    fi
} > "$CONFIG_FILE"
chmod 0644 "$CONFIG_FILE"

# ── Reload systemd + enable + start ─────────────────────────────────────────
log "reloading systemd"
systemctl daemon-reload

log "enabling and starting $SERVICE_NAME"
systemctl enable --now "$SERVICE_NAME"

# ── Done ─────────────────────────────────────────────────────────────────────
log "install complete"
systemctl status "$SERVICE_NAME" --no-pager --lines=5 || true

printf '\n'
log "next steps (run as %s, not root):" "$ENROLLING_USER"
printf '  zecurity-client setup\n'
printf '  zecurity-client login\n'
