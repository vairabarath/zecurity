#!/usr/bin/env bash
# connector-local-install.sh — ZECURITY connector local installer
#
# Installs:
#   - zecurity system user (no login, no shell)
#   - /usr/local/bin/zecurity-connector              (from local binary)
#   - /etc/zecurity/connector.conf                    (config, mode 0600)
#   - /var/lib/zecurity-connector/                    (state dir, zecurity-owned)
#   - /etc/systemd/system/zecurity-connector.service
#   - /etc/systemd/system/zecurity-connector-update.service
#   - /etc/systemd/system/zecurity-connector-update.timer
#
# Required environment variables:
#   CONTROLLER_ADDR       — gRPC address of the controller, e.g. "localhost:9090"
#   CONTROLLER_HTTP_ADDR  — HTTP address for /ca.crt fetch, e.g. "localhost:8080"
#   ENROLLMENT_TOKEN      — single-use JWT from the admin UI
#
# Usage:
#   sudo CONTROLLER_ADDR=... CONTROLLER_HTTP_ADDR=... ENROLLMENT_TOKEN=... \
#     ./scripts/connector-local-install.sh <path-to-binary>

set -euo pipefail

# ── Defaults ────────────────────────────────────────────────────────────────
SERVICE_USER="zecurity"
INSTALL_BIN="/usr/local/bin/zecurity-connector"
CONFIG_DIR="/etc/zecurity"
CONFIG_FILE="${CONFIG_DIR}/connector.conf"
STATE_DIR="/var/lib/zecurity-connector"
SYSTEMD_DIR="/etc/systemd/system"

# ── Helpers ─────────────────────────────────────────────────────────────────
log()  { printf '\033[1;34m[install]\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[install]\033[0m %s\n' "$*" >&2; }
err()  { printf '\033[1;31m[install]\033[0m %s\n' "$*" >&2; exit 1; }

usage() {
    cat <<EOF
Usage: $0 <path-to-binary>

Installs the ZECURITY connector locally.

Required env vars:
  CONTROLLER_ADDR       gRPC host:port for heartbeat (e.g. localhost:9090)
  CONTROLLER_HTTP_ADDR  HTTP host:port for /ca.crt   (e.g. localhost:8080)
  ENROLLMENT_TOKEN      Single-use JWT from admin UI
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

: "${CONTROLLER_ADDR:?CONTROLLER_ADDR is required}"
: "${CONTROLLER_HTTP_ADDR:?CONTROLLER_HTTP_ADDR is required}"
: "${ENROLLMENT_TOKEN:?ENROLLMENT_TOKEN is required}"

command -v systemctl >/dev/null || err "systemctl is required"

# ── Create system user ──────────────────────────────────────────────────────
if ! id "$SERVICE_USER" >/dev/null 2>&1; then
    log "creating system user: $SERVICE_USER"
    useradd --system \
        --shell /usr/sbin/nologin \
        --home-dir "$STATE_DIR" \
        --no-create-home \
        "$SERVICE_USER"
fi

# ── Create directories ──────────────────────────────────────────────────────
log "creating directories"
install -d -m 0755 "$CONFIG_DIR"
install -d -m 0700 -o "$SERVICE_USER" -g "$SERVICE_USER" "$STATE_DIR"

# ── Fetch intermediate CA certificate ───────────────────────────────────────
log "fetching /ca.crt from http://${CONTROLLER_HTTP_ADDR}"
if ! curl -fsSL --max-time 10 "http://${CONTROLLER_HTTP_ADDR}/ca.crt" -o "${CONFIG_DIR}/ca.crt"; then
    err "failed to fetch /ca.crt from controller"
fi
chmod 0644 "${CONFIG_DIR}/ca.crt"

# ── Install binary ──────────────────────────────────────────────────────────
log "installing binary to $INSTALL_BIN"
install -m 0755 -o root -g root "$LOCAL_BINARY" "$INSTALL_BIN"

# ── Install systemd units ───────────────────────────────────────────────────
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
UNITS_SRC="${REPO_ROOT}/connector/systemd"

if [[ ! -d "$UNITS_SRC" ]]; then
    err "systemd units not found in $UNITS_SRC"
fi

log "installing systemd units from $UNITS_SRC"
install -m 0644 "${UNITS_SRC}/zecurity-connector.service"        "${SYSTEMD_DIR}/"
install -m 0644 "${UNITS_SRC}/zecurity-connector-update.service" "${SYSTEMD_DIR}/"
install -m 0644 "${UNITS_SRC}/zecurity-connector-update.timer"   "${SYSTEMD_DIR}/"

# ── Write config file ───────────────────────────────────────────────────────
log "writing $CONFIG_FILE"
cat > "$CONFIG_FILE" <<EOF
# ZECURITY connector configuration — written by connector-local-install.sh
CONTROLLER_ADDR=${CONTROLLER_ADDR}
CONTROLLER_HTTP_ADDR=${CONTROLLER_HTTP_ADDR}
ENROLLMENT_TOKEN=${ENROLLMENT_TOKEN}
AUTO_UPDATE_ENABLED=false
LOG_LEVEL=info
STATE_DIR=${STATE_DIR}
EOF

chmod 0660 "$CONFIG_FILE"
chown root:"$SERVICE_USER" "$CONFIG_FILE"

# ── Reload systemd + enable + start ─────────────────────────────────────────
log "reloading systemd"
systemctl daemon-reload
systemctl enable --now zecurity-connector.service
# Update timer might fail if the update script isn't there or misconfigured for local, but we'll enable it anyway
systemctl enable --now zecurity-connector-update.timer || warn "failed to enable update timer"

log "install complete"
systemctl status zecurity-connector.service --no-pager --lines=5 || true
