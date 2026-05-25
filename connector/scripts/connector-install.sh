#!/usr/bin/env bash
# connector-install.sh — ZECURITY connector installer for Linux hosts
#
# Installs:
#   - zecurity system user (no login, no shell)
#   - /usr/local/bin/zecurity-connector
#   - /etc/zecurity/connector.conf                    (config, mode 0660)
#   - /var/lib/zecurity-connector/                    (state dir, zecurity-owned)
#   - /etc/systemd/system/zecurity-connector.service
#   - /etc/systemd/system/zecurity-connector-update.service
#   - /etc/systemd/system/zecurity-connector-update.timer
#
# Required environment variables:
#   CONTROLLER_ADDR       — gRPC address of the controller, e.g. "controller.example.com:9090"
#   CONTROLLER_HTTP_ADDR  — HTTP address for /ca.crt fetch, e.g. "controller.example.com:8080"
#   ENROLLMENT_TOKEN      — single-use JWT from the admin UI
#
# Optional:
#   CONNECTOR_LAN_ADDR    — address shields use to reach this connector (e.g. "192.168.1.10:9091")
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
#   curl -fsSL https://raw.githubusercontent.com/vairabarath/zecurity/main/connector/scripts/connector-install.sh | \
#     sudo CONTROLLER_ADDR=host:9090 ENROLLMENT_TOKEN=eyJ... bash

set -euo pipefail

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
  CONNECTOR_LAN_ADDR    LAN/internal IP shields use to reach this connector (e.g. 192.168.1.10:9091)
  GITHUB_REPO           default: vairabarath/zecurity
  CONNECTOR_VERSION     default: latest
  AUTO_UPDATE_ENABLED   default: false
EOF
}

while getopts ":fh" opt; do
    case "$opt" in
        f) FORCE=1 ;;
        h) usage; exit 0 ;;
        *) usage; exit 1 ;;
    esac
done

check_root() {
    [[ $EUID -eq 0 ]] || err "must run as root (use sudo)"
}

check_commands() {
    command -v curl      >/dev/null || err "curl is required"
    command -v systemctl >/dev/null || err "systemctl is required (systemd hosts only)"
    command -v sha256sum >/dev/null || err "sha256sum is required"
}

platform_binary() {
    local arch
    arch="$(uname -m)"
    case "$arch" in
        x86_64)  BINARY_NAME="connector-linux-amd64" ;;
        aarch64) BINARY_NAME="connector-linux-arm64" ;;
        *)       err "unsupported architecture: $arch" ;;
    esac
    log "detected architecture: ${arch} → ${BINARY_NAME}"
}

force_reinstall() {
    if [[ $FORCE -ne 1 ]]; then
        return 0
    fi

    log "force reinstall — stopping services and cleaning files"
    systemctl stop    zecurity-connector.service        2>/dev/null || true
    systemctl stop    zecurity-connector-update.timer   2>/dev/null || true
    systemctl stop    zecurity-connector-update.service 2>/dev/null || true
    systemctl disable zecurity-connector.service        2>/dev/null || true
    systemctl disable zecurity-connector-update.timer   2>/dev/null || true
    rm -f  "$INSTALL_BIN"
    rm -f  "$CONFIG_FILE"
    rm -f  "$SYSTEMD_DIR"/zecurity-connector.service
    rm -f  "$SYSTEMD_DIR"/zecurity-connector-update.service
    rm -f  "$SYSTEMD_DIR"/zecurity-connector-update.timer
    rm -rf "$STATE_DIR"
}

create_user() {
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
}

create_directories() {
    log "creating directories"
    install -d -m 0755                              "$CONFIG_DIR"
    install -d -m 0700 -o "$SERVICE_USER" -g "$SERVICE_USER" "$STATE_DIR"
}

fetch_ca_cert() {
    log "fetching /ca.crt from http://${CONTROLLER_HTTP_ADDR}"
    curl -fsSL --max-time 30 "http://${CONTROLLER_HTTP_ADDR}/ca.crt" -o "${CONFIG_DIR}/ca.crt" \
        || err "failed to fetch /ca.crt from controller"
    chmod 0644 "${CONFIG_DIR}/ca.crt"
    log "saved ${CONFIG_DIR}/ca.crt"
}

download_release() {
    TMP_DIR="$(mktemp -d)"
    trap 'rm -rf "$TMP_DIR"' EXIT

    if [[ "$CONNECTOR_VERSION" == "latest" ]]; then
        CONNECTOR_VERSION="$(curl -fsSL "https://api.github.com/repos/${GITHUB_REPO}/releases?per_page=50" \
            | grep '"tag_name"' | grep '"connector-v' \
            | sed 's/.*"tag_name": "connector-v\([^"]*\)".*/\1/' \
            | sort -V | tail -1 \
            | sed 's/^/connector-v/')"
        [[ -n "$CONNECTOR_VERSION" ]] || err "could not resolve latest connector release from GitHub API"
        log "resolved latest connector release: ${CONNECTOR_VERSION}"
    fi
    RELEASE_URL="https://github.com/${GITHUB_REPO}/releases/download/${CONNECTOR_VERSION}"

    log "downloading binary: ${RELEASE_URL}/${BINARY_NAME}"
    curl -fsSL --max-time 300 "${RELEASE_URL}/${BINARY_NAME}" -o "${TMP_DIR}/${BINARY_NAME}" \
        || err "failed to download binary from ${RELEASE_URL}/${BINARY_NAME}"

    log "downloading checksums: ${RELEASE_URL}/checksums.txt"
    curl -fsSL --max-time 30 "${RELEASE_URL}/checksums.txt" -o "${TMP_DIR}/checksums.txt" \
        || err "failed to download checksums.txt"
}

verify_checksum() {
    log "verifying SHA-256 checksum"
    EXPECTED="$(grep " ${BINARY_NAME}$" "${TMP_DIR}/checksums.txt" | awk '{print $1}')"
    [[ -n "$EXPECTED" ]] || err "no checksum entry for ${BINARY_NAME} in checksums.txt"

    ACTUAL="$(sha256sum "${TMP_DIR}/${BINARY_NAME}" | awk '{print $1}')"
    [[ "${EXPECTED,,}" == "${ACTUAL,,}" ]] \
        || err "SHA-256 MISMATCH — possible tampered binary! expected=${EXPECTED} actual=${ACTUAL}"

    log "checksum verified"
}

install_binary() {
    log "installing binary to $INSTALL_BIN"
    install -m 0755 -o root -g root "${TMP_DIR}/${BINARY_NAME}" "$INSTALL_BIN"
}

install_systemd_units() {
    local script_file script_dir units_src unit
    script_file="${BASH_SOURCE[0]:-}"
    units_src=""

    if [[ -n "$script_file" && -f "$script_file" ]]; then
        script_dir="$(cd "$(dirname "$script_file")" && pwd)"
        if [[ -d "${script_dir}/../systemd" ]]; then
            units_src="${script_dir}/../systemd"
            log "using systemd units from repo checkout: $units_src"
        fi
    fi

    if [[ -z "$units_src" ]]; then
        # Download units from the release (curl|bash install path).
        # BASH_SOURCE is unset when the script is piped via curl|bash, so
        # we fall back to fetching unit files from the same release as the binary.
        log "downloading systemd units from release"
        mkdir -p "${TMP_DIR}/systemd"
        for unit in zecurity-connector.service zecurity-connector-update.service zecurity-connector-update.timer; do
            curl -fsSL --max-time 30 "${RELEASE_URL}/${unit}" -o "${TMP_DIR}/systemd/${unit}" \
                || err "failed to download systemd unit: ${unit}"
        done
        units_src="${TMP_DIR}/systemd"
    fi

    install -m 0644 "${units_src}/zecurity-connector.service"        "${SYSTEMD_DIR}/"
    install -m 0644 "${units_src}/zecurity-connector-update.service" "${SYSTEMD_DIR}/"
    install -m 0644 "${units_src}/zecurity-connector-update.timer"   "${SYSTEMD_DIR}/"
}

write_config() {
    log "writing $CONFIG_FILE"
    cat > "$CONFIG_FILE" <<EOF
# ZECURITY connector configuration — written by connector-install.sh
# Read by systemd (EnvironmentFile=) and by the connector's figment loader.
# Permissions: 0660 root:zecurity

CONTROLLER_ADDR=${CONTROLLER_ADDR}
CONTROLLER_HTTP_ADDR=${CONTROLLER_HTTP_ADDR}
ENROLLMENT_TOKEN=${ENROLLMENT_TOKEN}
AUTO_UPDATE_ENABLED=${AUTO_UPDATE_ENABLED}
LOG_LEVEL=info
STATE_DIR=${STATE_DIR}
EOF

    # Append optional vars cleanly — avoids a blank line in the config when unset.
    if [[ -n "${CONNECTOR_LAN_ADDR:-}" ]]; then
        printf 'CONNECTOR_LAN_ADDR=%s\n' "${CONNECTOR_LAN_ADDR}" >> "$CONFIG_FILE"
    fi

    # 0660: root reads/writes via EnvironmentFile= before service start;
    # zecurity group reads/writes so the connector process (run as zecurity) can
    # rewrite the file after enrollment to remove ENROLLMENT_TOKEN.
    chmod 0660 "$CONFIG_FILE"
    chown root:"$SERVICE_USER" "$CONFIG_FILE"
}

enable_service() {
    log "reloading systemd"
    systemctl daemon-reload

    log "enabling and starting zecurity-connector.service"
    systemctl enable --now zecurity-connector.service

    log "enabling zecurity-connector-update.timer"
    systemctl enable --now zecurity-connector-update.timer
}

final_status() {
    log "install complete"
    echo ""
    systemctl status zecurity-connector.service --no-pager --lines=10 || true
    echo ""
    log "tail logs with: journalctl -u zecurity-connector -f"
}

main() {
    check_root
    : "${CONTROLLER_ADDR:?CONTROLLER_ADDR is required}"
    : "${CONTROLLER_HTTP_ADDR:?CONTROLLER_HTTP_ADDR is required}"
    : "${ENROLLMENT_TOKEN:?ENROLLMENT_TOKEN is required}"
    check_commands
    platform_binary
    force_reinstall
    create_user
    create_directories
    fetch_ca_cert
    download_release
    verify_checksum
    install_binary
    install_systemd_units
    write_config
    enable_service
    final_status
}

main "$@"
