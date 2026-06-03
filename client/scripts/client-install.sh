#!/usr/bin/env bash
# client-install.sh — ZECURITY client installer for Linux hosts
#
# Installs:
#   - /usr/local/bin/zecurity-client              (the binary)
#   - /etc/zecurity/client.conf                   (system-level config, optional)
#   - /etc/systemd/system/zecurity-client.service (daemon unit)
#
# Zero-config: no required environment variables.
# Run then configure with: zecurity-client setup --workspace <name>
#
# Optional environment variables:
#   CONTROLLER_ADDR       — pre-configure gRPC address  (e.g. "192.168.1.10:9090")
#   CONTROLLER_HTTP_ADDR  — pre-configure HTTP base URL  (e.g. "192.168.1.10:8080")
#   GITHUB_REPO           — override release source      (default: vairabarath/zecurity)
#   CLIENT_VERSION        — pin a specific version tag   (default: latest)
#
# Flags:
#   -f    Force reinstall (stop service, remove files, re-run all steps)
#   -h    Show help
#
# Typical usage:
#   curl -fsSL https://raw.githubusercontent.com/vairabarath/zecurity/main/client/scripts/client-install.sh | sudo bash
#
#   With pre-configured controller (LAN install):
#   curl -fsSL ... | sudo CONTROLLER_ADDR=192.168.1.10:9090 bash

set -euo pipefail

# ── Defaults ────────────────────────────────────────────────────────────────
GITHUB_REPO="${GITHUB_REPO:-vairabarath/zecurity}"
CLIENT_VERSION="${CLIENT_VERSION:-latest}"
INSTALL_BIN="/usr/local/bin/zecurity-client"
CONFIG_DIR="/etc/zecurity"
CONFIG_FILE="${CONFIG_DIR}/client.conf"
SYSTEMD_DIR="/etc/systemd/system"
FORCE=0

# ── Helpers ─────────────────────────────────────────────────────────────────
log()  { printf '\033[1;34m[install]\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[install]\033[0m %s\n' "$*" >&2; }
err()  { printf '\033[1;31m[install]\033[0m %s\n' "$*" >&2; exit 1; }

usage() {
    cat <<EOF
Usage: $0 [-f] [-h]

Installs the ZECURITY client daemon as a systemd service.

Flags:
  -f    Force reinstall (stop, remove, re-install)
  -h    Show this help

Optional env vars:
  CONTROLLER_ADDR       gRPC host:port  (e.g. host:9090)
  CONTROLLER_HTTP_ADDR  HTTP host:port  (e.g. host:8080)
  GITHUB_REPO           default: vairabarath/zecurity
  CLIENT_VERSION        default: latest

After install, configure and log in:
  zecurity-client setup --workspace <workspace-name>
  zecurity-client login
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

# checks is this root or not

[[ $EUID -eq 0 ]] || err "must run as root (use sudo)"


# these are for the version checking if it is missing exits installer

command -v curl     >/dev/null || err "curl is required"
command -v systemctl >/dev/null || err "systemctl is required (systemd hosts only)"
command -v sha256sum >/dev/null || err "sha256sum is required"

# Detect the real user who invoked sudo (the daemon will run as this user).
INSTALL_USER="${SUDO_USER:-}"
if [[ -z "$INSTALL_USER" ]]; then
    INSTALL_USER="$(logname 2>/dev/null || whoami)"
fi
[[ -n "$INSTALL_USER" && "$INSTALL_USER" != "root" ]] || \
    err "could not detect non-root install user — run via: sudo bash client-install.sh"
log "installing for user: $INSTALL_USER"

# ── Platform detection ──────────────────────────────────────────────────────
ARCH="$(uname -m)"
case "$ARCH" in
    x86_64)  BINARY_NAME="client-linux-amd64" ;;
    aarch64) BINARY_NAME="client-linux-arm64" ;;
    *)       err "unsupported architecture: $ARCH" ;;
esac
log "detected architecture: $ARCH → $BINARY_NAME"

# ── Force reinstall — stop + clean ──────────────────────────────────────────
if [[ $FORCE -eq 1 ]]; then
    log "force reinstall — stopping service and cleaning files"
    systemctl stop zecurity-client.service 2>/dev/null || true
    systemctl disable zecurity-client.service 2>/dev/null || true
    rm -f "$INSTALL_BIN"
    rm -f "${SYSTEMD_DIR}/zecurity-client.service"
fi

# ── Download release artifacts ──────────────────────────────────────────────
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

if [[ "$CLIENT_VERSION" == "latest" ]]; then
    CLIENT_VERSION="$(curl -fsSL "https://api.github.com/repos/${GITHUB_REPO}/releases?per_page=50" \
        | grep '"tag_name"' | grep '"client-v' \
        | sed 's/.*"tag_name": "client-v\([^"]*\)".*/\1/' \
        | sort -V | tail -1 \
        | sed 's/^/client-v/')"
    [[ -n "$CLIENT_VERSION" ]] || err "could not resolve latest client release from GitHub API"
    log "resolved latest client release: ${CLIENT_VERSION}"
fi
RELEASE_URL="https://github.com/${GITHUB_REPO}/releases/download/${CLIENT_VERSION}"

log "downloading binary: ${RELEASE_URL}/${BINARY_NAME}"
curl -fsSL --max-time 300 "${RELEASE_URL}/${BINARY_NAME}" -o "${TMP_DIR}/${BINARY_NAME}" \
    || err "failed to download binary"

log "downloading checksums: ${RELEASE_URL}/checksums.txt"
curl -fsSL --max-time 30 "${RELEASE_URL}/checksums.txt" -o "${TMP_DIR}/checksums.txt" \
    || err "failed to download checksums.txt"

# ── Verify SHA-256 ──────────────────────────────────────────────────────────
log "verifying SHA-256 checksum"
EXPECTED="$(grep " ${BINARY_NAME}$" "${TMP_DIR}/checksums.txt" | awk '{print $1}')"
[[ -n "$EXPECTED" ]] || err "no checksum entry for ${BINARY_NAME} in checksums.txt"
ACTUAL="$(sha256sum "${TMP_DIR}/${BINARY_NAME}" | awk '{print $1}')"
[[ "${EXPECTED,,}" == "${ACTUAL,,}" ]] || \
    err "SHA-256 MISMATCH — possible tampered binary! expected=${EXPECTED} actual=${ACTUAL}"
log "checksum verified"

# ── Install binary ──────────────────────────────────────────────────────────
log "installing binary to $INSTALL_BIN"
install -m 0755 -o root -g root "${TMP_DIR}/${BINARY_NAME}" "$INSTALL_BIN"

# ── Install systemd unit ─────────────────────────────────────────────────────
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
    log "downloading systemd unit from release"
    mkdir -p "${TMP_DIR}/systemd"
    curl -fsSL --max-time 30 "${RELEASE_URL}/zecurity-client.service" \
        -o "${TMP_DIR}/systemd/zecurity-client.service" \
        || err "failed to download systemd unit"
    UNITS_SRC="${TMP_DIR}/systemd"
fi

# Patch User= to the installing user.
SERVICE_TMP="${TMP_DIR}/zecurity-client.service"
sed "s/^User=$/User=${INSTALL_USER}/" "${UNITS_SRC}/zecurity-client.service" > "$SERVICE_TMP"
install -m 0644 "$SERVICE_TMP" "${SYSTEMD_DIR}/zecurity-client.service"

# ── Write system config (optional — only if addresses provided) ──────────────
install -d -m 0755 "$CONFIG_DIR"

if [[ -n "${CONTROLLER_ADDR:-}" || -n "${CONTROLLER_HTTP_ADDR:-}" ]]; then
    log "writing system config: $CONFIG_FILE"
    {
        echo "# ZECURITY client system config — written by client-install.sh"
        [[ -n "${CONTROLLER_ADDR:-}" ]]      && echo "controller_address = \"${CONTROLLER_ADDR}\""
        [[ -n "${CONTROLLER_HTTP_ADDR:-}" ]] && echo "http_base_url = \"http://${CONTROLLER_HTTP_ADDR}\""
        echo "workspace = \"\""
    } > "$CONFIG_FILE"
    chmod 0644 "$CONFIG_FILE"
    log "controller pre-configured — run: zecurity-client setup --workspace <name>"
else
    log "no CONTROLLER_ADDR set — run: zecurity-client setup --workspace <name> --controller <host:port>"
fi

# ── Reload systemd + enable + start ─────────────────────────────────────────
log "reloading systemd"
systemctl daemon-reload

log "enabling and starting zecurity-client.service"
systemctl enable --now zecurity-client.service

# ── Final status ────────────────────────────────────────────────────────────
log "install complete"
echo ""
systemctl status zecurity-client.service --no-pager --lines=10 || true
echo ""
log "next steps:"
log "  1. zecurity-client setup --workspace <workspace-name> [--controller host:port]"
log "  2. zecurity-client login"
log "  3. zecurity-client up"
log ""
log "tail logs with: journalctl -u zecurity-client -f"
