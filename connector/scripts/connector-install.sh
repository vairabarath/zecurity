#!/usr/bin/env bash
# connector-install.sh — ZECURITY connector installer for Linux hosts
#
# Installs:
#   - zecurity system user (no login, no shell)
#   - /usr/local/bin/zecurity-connector              (the binary)
#   - /etc/zecurity/connector.conf                    (config, mode 0600)
#   - /var/lib/zecurity-connector/                    (state dir, zecurity-owned)
#   - /etc/systemd/system/zecurity-connector.service
#   - /etc/systemd/system/zecurity-connector-update.service
#   - /etc/systemd/system/zecurity-connector-update.timer
#
# Required environment variables (set before calling):
#   CONTROLLER_ADDR       — gRPC address of the controller, e.g. "controller.example.com:9090"
#   CONTROLLER_HTTP_ADDR  — HTTP address for /ca.crt fetch, e.g. "controller.example.com:8080"
#   ENROLLMENT_TOKEN      — single-use JWT from the admin UI
#
# Optional:
#   AGENT_ADDR            — address shields use to reach this connector (e.g. "192.168.1.10:9091")
#                           Set this to the LAN/internal IP when shields are on the same network.
#                           If unset, the controller falls back to the connector's public IP.
#   GITHUB_REPO           — override release source (default: vairabarath/zecurity)
#   CONNECTOR_VERSION     — specific version tag (default: latest)
#   AUTO_UPDATE_ENABLED   — default: false
#
# Flags:
#   -f    Force reinstall (stop services, remove files, re-run all steps)
#   -h    Show help
#
# Typical usage (from the install command shown in the admin UI):
#   curl -fsSL https://github.com/vairabarath/zecurity/releases/latest/download/connector-install.sh | \
#     sudo CONTROLLER_ADDR=host:9090 ENROLLMENT_TOKEN=eyJ... bash

set -euo pipefail

# ── Defaults ────────────────────────────────────────────────────────────────
GITHUB_REPO="${GITHUB_REPO:-vairabarath/zecurity}"
CONNECTOR_VERSION="${CONNECTOR_VERSION:-latest}"
AUTO_UPDATE_ENABLED="${AUTO_UPDATE_ENABLED:-false}"
SERVICE_USER="zecurity"
INSTALL_BIN="/usr/local/bin/zecurity-connector"
CONFIG_DIR="/etc/zecurity"
CONFIG_FILE="${CONFIG_DIR}/connector.conf"
STATE_DIR="/var/lib/zecurity-connector"
SYSTEMD_DIR="/etc/systemd/system"

FORCE=0

# ── Helpers ─────────────────────────────────────────────────────────────────
log()  { printf '\033[1;34m[install]\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[install]\033[0m %s\n' "$*" >&2; }
err()  { printf '\033[1;31m[install]\033[0m %s\n' "$*" >&2; exit 1; }

usage() {
    cat <<EOF
Usage: $0 [-f] [-h]

Installs the ZECURITY connector as a systemd service.

Flags:
  -f    Force reinstall (stop, remove, re-install)
  -h    Show this help

Required env vars:
  CONTROLLER_ADDR       gRPC host:port for heartbeat (e.g. host:9090)
  CONTROLLER_HTTP_ADDR  HTTP host:port for /ca.crt   (e.g. host:8080)
  ENROLLMENT_TOKEN      Single-use JWT from admin UI

Optional env vars:
  GITHUB_REPO           default: vairabarath/zecurity
  CONNECTOR_VERSION     default: latest
  AUTO_UPDATE_ENABLED   default: false
EOF
}

# ── Parse flags ─────────────────────────────────────────────────────────────
while getopts ":fh" opt; do
    case "$opt" in
        f) FORCE=1 ;;
        h) usage; exit 0 ;;
        *) usage; exit 1 ;;
    esac
done

# ── Checks ──────────────────────────────────────────────────────────────────
[[ $EUID -eq 0 ]] || err "must run as root (use sudo)"

: "${CONTROLLER_ADDR:?CONTROLLER_ADDR is required}"
: "${CONTROLLER_HTTP_ADDR:?CONTROLLER_HTTP_ADDR is required}"
: "${ENROLLMENT_TOKEN:?ENROLLMENT_TOKEN is required}"

command -v curl >/dev/null || err "curl is required"
command -v systemctl >/dev/null || err "systemctl is required (this installer supports systemd hosts only)"
command -v sha256sum >/dev/null || err "sha256sum is required"

# ── Platform detection ──────────────────────────────────────────────────────
ARCH="$(uname -m)"
case "$ARCH" in
    x86_64)  BINARY_NAME="connector-linux-amd64" ;;
    aarch64) BINARY_NAME="connector-linux-arm64" ;;
    *)       err "unsupported architecture: $ARCH" ;;
esac
log "detected architecture: $ARCH → $BINARY_NAME"

# ── Force reinstall — stop + clean ──────────────────────────────────────────
if [[ $FORCE -eq 1 ]]; then
    log "force reinstall — stopping services and cleaning files"
    systemctl stop zecurity-connector.service 2>/dev/null || true
    systemctl stop zecurity-connector-update.timer 2>/dev/null || true
    systemctl stop zecurity-connector-update.service 2>/dev/null || true
    systemctl disable zecurity-connector.service 2>/dev/null || true
    systemctl disable zecurity-connector-update.timer 2>/dev/null || true
    rm -f "$INSTALL_BIN"
    rm -f "$CONFIG_FILE"
    rm -f "$SYSTEMD_DIR"/zecurity-connector.service
    rm -f "$SYSTEMD_DIR"/zecurity-connector-update.service
    rm -f "$SYSTEMD_DIR"/zecurity-connector-update.timer
    rm -rf "$STATE_DIR"
fi

# ── Create system user ──────────────────────────────────────────────────────
if ! id "$SERVICE_USER" >/dev/null 2>&1; then
    log "creating system user: $SERVICE_USER"
    useradd --system \
        --shell /usr/sbin/nologin \
        --home-dir "$STATE_DIR" \
        --no-create-home \
        "$SERVICE_USER"
else
    log "user $SERVICE_USER already exists"
fi

# ── Create directories ──────────────────────────────────────────────────────
log "creating directories"
install -d -m 0755 "$CONFIG_DIR"
install -d -m 0700 -o "$SERVICE_USER" -g "$SERVICE_USER" "$STATE_DIR"

# ── Fetch intermediate CA certificate ───────────────────────────────────────
log "fetching /ca.crt from http://${CONTROLLER_HTTP_ADDR}"
if ! curl -fsSL --max-time 30 "http://${CONTROLLER_HTTP_ADDR}/ca.crt" -o "${CONFIG_DIR}/ca.crt"; then
    err "failed to fetch /ca.crt from controller"
fi
chmod 0644 "${CONFIG_DIR}/ca.crt"
log "saved ${CONFIG_DIR}/ca.crt"

# ── Download release artifacts ──────────────────────────────────────────────
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

if [[ "$CONNECTOR_VERSION" == "latest" ]]; then
    CONNECTOR_VERSION="$(curl -fsSL "https://api.github.com/repos/${GITHUB_REPO}/releases" \
        | grep '"tag_name"' | grep '"connector-v' | head -1 \
        | sed 's/.*"tag_name": "\(.*\)".*/\1/')"
    [[ -n "$CONNECTOR_VERSION" ]] || err "could not resolve latest connector release from GitHub API"
    log "resolved latest connector release: ${CONNECTOR_VERSION}"
fi
RELEASE_URL="https://github.com/${GITHUB_REPO}/releases/download/${CONNECTOR_VERSION}"

log "downloading binary: ${RELEASE_URL}/${BINARY_NAME}"
if ! curl -fsSL --max-time 300 "${RELEASE_URL}/${BINARY_NAME}" -o "${TMP_DIR}/${BINARY_NAME}"; then
    err "failed to download binary from ${RELEASE_URL}/${BINARY_NAME}"
fi

log "downloading checksums: ${RELEASE_URL}/checksums.txt"
if ! curl -fsSL --max-time 30 "${RELEASE_URL}/checksums.txt" -o "${TMP_DIR}/checksums.txt"; then
    err "failed to download checksums.txt"
fi

# ── Verify SHA-256 ──────────────────────────────────────────────────────────
log "verifying SHA-256 checksum"
EXPECTED="$(grep " ${BINARY_NAME}$" "${TMP_DIR}/checksums.txt" | awk '{print $1}')"
if [[ -z "$EXPECTED" ]]; then
    err "no checksum entry for ${BINARY_NAME} in checksums.txt"
fi

ACTUAL="$(sha256sum "${TMP_DIR}/${BINARY_NAME}" | awk '{print $1}')"
if [[ "${EXPECTED,,}" != "${ACTUAL,,}" ]]; then
    err "SHA-256 MISMATCH — possible tampered binary! expected=${EXPECTED} actual=${ACTUAL}"
fi
log "checksum verified"

# ── Install binary ──────────────────────────────────────────────────────────
log "installing binary to $INSTALL_BIN"
install -m 0755 -o root -g root "${TMP_DIR}/${BINARY_NAME}" "$INSTALL_BIN"

# ── Install systemd units ───────────────────────────────────────────────────
# Two possible sources for systemd unit files:
#   1. Repo checkout  — units live at ../systemd relative to this script.
#                        Detected via BASH_SOURCE (only set when bash reads the
#                        script from a file on disk, NOT when piped via curl|bash).
#   2. Release download — fetched from the same release as the binary.
#                        This path is taken when the install command is:
#                        curl -fsSL ... | sudo ... bash
#
# `set -u` is active, so use `${BASH_SOURCE[0]:-}` to get an empty default
# instead of erroring on the unbound array element when piped.
SCRIPT_FILE="${BASH_SOURCE[0]:-}"
UNITS_SRC=""

if [[ -n "$SCRIPT_FILE" && -f "$SCRIPT_FILE" ]]; then
    SCRIPT_DIR="$(cd "$(dirname "$SCRIPT_FILE")" && pwd)"
    if [[ -d "${SCRIPT_DIR}/../systemd" ]]; then
        UNITS_SRC="${SCRIPT_DIR}/../systemd"
        log "using systemd units from repo checkout: $UNITS_SRC"
    fi
fi

if [[ -z "$UNITS_SRC" ]]; then
    # Download units from the release (curl|bash install path).
    log "downloading systemd units from release"
    mkdir -p "${TMP_DIR}/systemd"
    for unit in zecurity-connector.service zecurity-connector-update.service zecurity-connector-update.timer; do
        if ! curl -fsSL --max-time 30 "${RELEASE_URL}/${unit}" -o "${TMP_DIR}/systemd/${unit}"; then
            err "failed to download systemd unit: ${unit}"
        fi
    done
    UNITS_SRC="${TMP_DIR}/systemd"
fi

install -m 0644 "${UNITS_SRC}/zecurity-connector.service"        "${SYSTEMD_DIR}/"
install -m 0644 "${UNITS_SRC}/zecurity-connector-update.service" "${SYSTEMD_DIR}/"
install -m 0644 "${UNITS_SRC}/zecurity-connector-update.timer"   "${SYSTEMD_DIR}/"

# ── Write config file ───────────────────────────────────────────────────────
log "writing $CONFIG_FILE"
cat > "$CONFIG_FILE" <<EOF
# ZECURITY connector configuration — written by connector-install.sh
# This file is read by systemd (EnvironmentFile=) and by the connector's figment loader.
# Permissions: 0600 root:zecurity

CONTROLLER_ADDR=${CONTROLLER_ADDR}
CONTROLLER_HTTP_ADDR=${CONTROLLER_HTTP_ADDR}
ENROLLMENT_TOKEN=${ENROLLMENT_TOKEN}
AUTO_UPDATE_ENABLED=${AUTO_UPDATE_ENABLED}
LOG_LEVEL=info
STATE_DIR=${STATE_DIR}
$([ -n "${AGENT_ADDR:-}" ] && echo "AGENT_ADDR=${AGENT_ADDR}")
EOF

# 0660 = root owner can read/write, zecurity group can read/write.
# The service process reads this file via figment's Toml loader in config.rs
# and writes to it after enrollment to remove ENROLLMENT_TOKEN.
# systemd reads it as root via EnvironmentFile= before starting the service.
chmod 0660 "$CONFIG_FILE"
chown root:"$SERVICE_USER" "$CONFIG_FILE"

# ── Reload systemd + enable + start ─────────────────────────────────────────
log "reloading systemd"
systemctl daemon-reload

log "enabling and starting zecurity-connector.service"
systemctl enable --now zecurity-connector.service

log "enabling zecurity-connector-update.timer"
systemctl enable --now zecurity-connector-update.timer

# ── Final status ────────────────────────────────────────────────────────────
log "install complete"
echo ""
systemctl status zecurity-connector.service --no-pager --lines=10 || true
echo ""
log "tail logs with: journalctl -u zecurity-connector -f"
