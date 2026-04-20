#!/usr/bin/env bash
# shield-install.sh — ZECURITY shield installer for Linux hosts
#
# Installs:
#   - zecurity system user (no login, no shell)
#   - /usr/local/bin/zecurity-shield
#   - /etc/zecurity/shield.conf
#   - /var/lib/zecurity-shield/
#   - /etc/systemd/system/zecurity-shield.service
#   - /etc/systemd/system/zecurity-shield-update.service
#   - /etc/systemd/system/zecurity-shield-update.timer
#
# Required environment variables:
#   CONTROLLER_ADDR       — controller gRPC address, e.g. "controller.example.com:9090"
#   CONTROLLER_HTTP_ADDR  — controller HTTP address, e.g. "controller.example.com:8080"
#   ENROLLMENT_TOKEN      — single-use JWT from the admin UI

set -euo pipefail

GITHUB_REPO="${GITHUB_REPO:-vairabarath/zecurity}"
SHIELD_VERSION="${SHIELD_VERSION:-latest}"
AUTO_UPDATE_ENABLED="${AUTO_UPDATE_ENABLED:-false}"
SERVICE_USER="zecurity"
INSTALL_BIN="/usr/local/bin/zecurity-shield"
CONFIG_DIR="/etc/zecurity"
CONFIG_FILE="${CONFIG_DIR}/shield.conf"
STATE_DIR="/var/lib/zecurity-shield"
SYSTEMD_DIR="/etc/systemd/system"
DOCS_URL="https://wiki.nftables.org"

FORCE=0
OS=""
OS_VERSION=""

log()  { printf '\033[1;34m[install]\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[install]\033[0m %s\n' "$*" >&2; }
err()  { printf '\033[1;31m[install]\033[0m %s\n' "$*" >&2; exit 1; }

usage() {
    cat <<EOF
Usage: $0 [-f] [-h]

Installs the ZECURITY shield as a systemd service.

Flags:
  -f    Force reinstall (stop, remove, re-install)
  -h    Show this help

Required env vars:
  CONTROLLER_ADDR       gRPC host:port for enrollment (e.g. host:9090)
  CONTROLLER_HTTP_ADDR  HTTP host:port for /ca.crt   (e.g. host:8080)
  ENROLLMENT_TOKEN      Single-use JWT from admin UI

Optional env vars:
  GITHUB_REPO           default: vairabarath/zecurity
  SHIELD_VERSION        default: latest
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

detect_os() {
    if [[ -f /etc/os-release ]]; then
        # shellcheck disable=SC1091
        . /etc/os-release
        OS="${ID:-}"
        OS_VERSION="${VERSION_ID:-}"
    else
        err "cannot detect OS: /etc/os-release not found"
    fi

    [[ -n "$OS" ]] || err "cannot detect OS: missing ID in /etc/os-release"
    log "detected OS: ${OS}${OS_VERSION:+ ${OS_VERSION}}"
}

check_kernel() {
    local kernel major minor
    kernel="$(uname -r | cut -d. -f1-2)"
    major="$(printf '%s' "$kernel" | cut -d. -f1)"
    minor="$(printf '%s' "$kernel" | cut -d. -f2)"

    if [[ "$major" -lt 3 ]] || { [[ "$major" -eq 3 ]] && [[ "$minor" -lt 13 ]]; }; then
        err "kernel ${kernel} is too old: nftables requires 3.13+"
    fi

    log "kernel ${kernel} supports nftables"
}

ensure_nftables() {
    if command -v nft >/dev/null 2>&1; then
        log "nft found: $(nft --version)"
        return 0
    fi

    log "nft not found — installing nftables package"

    case "$OS" in
        ubuntu|debian|linuxmint|pop)
            apt-get update -qq
            apt-get install -y nftables
            ;;
        rhel|centos|rocky|almalinux)
            if command -v dnf >/dev/null 2>&1; then
                dnf install -y nftables
            else
                yum install -y nftables
            fi
            ;;
        fedora)
            dnf install -y nftables
            ;;
        arch|manjaro)
            pacman -Sy --noconfirm nftables
            ;;
        opensuse*|sles)
            zypper install -y nftables
            ;;
        *)
            err "unsupported OS '${OS}' — install nftables manually: ${DOCS_URL}"
            ;;
    esac

    command -v nft >/dev/null 2>&1 || err "nftables installation failed"
    log "installed nftables: $(nft --version)"
}

check_nftables_service() {
    if systemctl is-active --quiet nftables 2>/dev/null; then
        warn "system nftables service is active"
        warn "it may flush rules and reload /etc/nftables.conf on boot"
        warn "Zecurity Shield recreates its own 'inet zecurity' table on startup,"
        warn "but you should ensure your host firewall config does not remove it permanently"
    fi
}

check_commands() {
    command -v curl >/dev/null || err "curl is required"
    command -v systemctl >/dev/null || err "systemctl is required (systemd hosts only)"
    command -v sha256sum >/dev/null || err "sha256sum is required"
}

platform_binary() {
    local arch
    arch="$(uname -m)"
    case "$arch" in
        x86_64)  BINARY_NAME="shield-linux-amd64" ;;
        aarch64) BINARY_NAME="shield-linux-arm64" ;;
        *)       err "unsupported architecture: $arch" ;;
    esac
    log "detected architecture: ${arch} -> ${BINARY_NAME}"
}

force_reinstall() {
    if [[ $FORCE -ne 1 ]]; then
        return 0
    fi

    log "force reinstall — stopping services and cleaning files"
    systemctl stop zecurity-shield.service 2>/dev/null || true
    systemctl stop zecurity-shield-update.timer 2>/dev/null || true
    systemctl stop zecurity-shield-update.service 2>/dev/null || true
    systemctl disable zecurity-shield.service 2>/dev/null || true
    systemctl disable zecurity-shield-update.timer 2>/dev/null || true
    rm -f "$INSTALL_BIN"
    rm -f "$CONFIG_FILE"
    rm -f "$SYSTEMD_DIR"/zecurity-shield.service
    rm -f "$SYSTEMD_DIR"/zecurity-shield-update.service
    rm -f "$SYSTEMD_DIR"/zecurity-shield-update.timer
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
    install -d -m 0755 "$CONFIG_DIR"
    install -d -m 0700 -o "$SERVICE_USER" -g "$SERVICE_USER" "$STATE_DIR"
}

fetch_ca_cert() {
    log "fetching /ca.crt from http://${CONTROLLER_HTTP_ADDR}"
    if ! curl -fsSL --max-time 30 "http://${CONTROLLER_HTTP_ADDR}/ca.crt" -o "${CONFIG_DIR}/ca.crt"; then
        err "failed to fetch /ca.crt from controller"
    fi
    chmod 0644 "${CONFIG_DIR}/ca.crt"
    log "saved ${CONFIG_DIR}/ca.crt"
}

download_release() {
    TMP_DIR="$(mktemp -d)"
    trap 'rm -rf "$TMP_DIR"' EXIT

    if [[ "$SHIELD_VERSION" == "latest" ]]; then
        SHIELD_VERSION="$(curl -fsSL "https://api.github.com/repos/${GITHUB_REPO}/releases" \
            | grep '"tag_name"' | grep '"shield-v' | head -1 \
            | sed 's/.*"tag_name": "\(.*\)".*/\1/')"
        [[ -n "$SHIELD_VERSION" ]] || err "could not resolve latest shield release from GitHub API"
        log "resolved latest shield release: ${SHIELD_VERSION}"
    fi
    RELEASE_URL="https://github.com/${GITHUB_REPO}/releases/download/${SHIELD_VERSION}"

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
        || err "SHA-256 MISMATCH — expected=${EXPECTED} actual=${ACTUAL}"

    log "checksum verified"
}

install_binary() {
    log "installing binary to $INSTALL_BIN"
    install -m 0755 -o root -g root "${TMP_DIR}/${BINARY_NAME}" "$INSTALL_BIN"
}

install_systemd_units() {
    local script_file units_src script_dir unit
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
        log "downloading systemd units from release"
        mkdir -p "${TMP_DIR}/systemd"
        for unit in zecurity-shield.service zecurity-shield-update.service zecurity-shield-update.timer; do
            curl -fsSL --max-time 30 "${RELEASE_URL}/${unit}" -o "${TMP_DIR}/systemd/${unit}" \
                || err "failed to download systemd unit: ${unit}"
        done
        units_src="${TMP_DIR}/systemd"
    fi

    install -m 0644 "${units_src}/zecurity-shield.service"        "${SYSTEMD_DIR}/"
    install -m 0644 "${units_src}/zecurity-shield-update.service" "${SYSTEMD_DIR}/"
    install -m 0644 "${units_src}/zecurity-shield-update.timer"   "${SYSTEMD_DIR}/"
}

write_config() {
    log "writing $CONFIG_FILE"
    cat > "$CONFIG_FILE" <<EOF
# ZECURITY shield configuration — written by shield-install.sh
# Read by systemd (EnvironmentFile=) and by the shield's figment loader.

CONTROLLER_ADDR=${CONTROLLER_ADDR}
CONTROLLER_HTTP_ADDR=${CONTROLLER_HTTP_ADDR}
ENROLLMENT_TOKEN=${ENROLLMENT_TOKEN}
AUTO_UPDATE_ENABLED=${AUTO_UPDATE_ENABLED}
LOG_LEVEL=info
STATE_DIR=${STATE_DIR}
EOF

    chmod 0660 "$CONFIG_FILE"
    chown root:"$SERVICE_USER" "$CONFIG_FILE"
}

enable_service() {
    log "reloading systemd"
    systemctl daemon-reload

    log "enabling and starting zecurity-shield.service"
    systemctl enable --now zecurity-shield.service

    log "enabling zecurity-shield-update.timer"
    systemctl enable --now zecurity-shield-update.timer
}

final_status() {
    log "install complete"
    echo ""
    systemctl status zecurity-shield.service --no-pager --lines=10 || true
    echo ""
    log "tail logs with: journalctl -u zecurity-shield -f"
}

main() {
    check_root
    : "${CONTROLLER_ADDR:?CONTROLLER_ADDR is required}"
    : "${CONTROLLER_HTTP_ADDR:?CONTROLLER_HTTP_ADDR is required}"
    : "${ENROLLMENT_TOKEN:?ENROLLMENT_TOKEN is required}"
    check_commands
    detect_os
    check_kernel
    ensure_nftables
    check_nftables_service
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
