#!/usr/bin/env bash
# client-uninstall.sh — ZECURITY client uninstaller for Linux hosts
#
# Reverses everything client-local-install.sh set up:
#   - stops + disables zecurity-client.service
#   - removes /etc/systemd/system/zecurity-client.service
#   - removes /usr/local/bin/zecurity-client
#   - removes /etc/zecurity/client.conf  (unless --keep-config)
#   - removes ~/.local/share/zecurity-client/  (user state + encrypted keys)
#     for the specified user  (unless --keep-state)
#
# Default behavior PROMPTS before destructive actions. Use -y to skip prompts.
#
# Flags:
#   -u <user>       User whose state dir to remove (default: $SUDO_USER)
#   -y              Non-interactive: answer yes to all prompts
#   --keep-config   Preserve /etc/zecurity/client.conf
#   --keep-state    Preserve ~/.local/share/zecurity-client/ (encrypted keys + workspace state)
#   -h              Show help
#
# Usage:
#   sudo ./scripts/client-uninstall.sh                   # interactive, full removal
#   sudo ./scripts/client-uninstall.sh -y                # no prompts
#   sudo ./scripts/client-uninstall.sh -y --keep-state   # leave encrypted state intact
#   sudo ./scripts/client-uninstall.sh -u alice -y       # remove for specific user

set -euo pipefail

# ── Defaults ────────────────────────────────────────────────────────────────
INSTALL_BIN="/usr/local/bin/zecurity-client"
CONFIG_FILE="/etc/zecurity/client.conf"
SYSTEMD_DIR="/etc/systemd/system"
SERVICE_NAME="zecurity-client.service"

ENROLLING_USER="${SUDO_USER:-}"
ASSUME_YES=0
KEEP_CONFIG=0
KEEP_STATE=0

# ── Helpers ─────────────────────────────────────────────────────────────────
log()  { printf '\033[1;34m[uninstall]\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[uninstall]\033[0m %s\n' "$*" >&2; }
err()  { printf '\033[1;31m[uninstall]\033[0m %s\n' "$*" >&2; exit 1; }

confirm() {
    if [[ $ASSUME_YES -eq 1 ]]; then
        return 0
    fi
    local reply
    read -r -p "$(printf '\033[1;33m[uninstall]\033[0m %s [y/N] ' "$1")" reply
    [[ "$reply" =~ ^[Yy]$ ]]
}

usage() {
    cat <<EOF
Usage: sudo $0 [-y] [-u <user>] [--keep-config] [--keep-state] [-h]

Reverses the ZECURITY client installation.

Flags:
  -u <user>       User whose state dir to remove (default: \$SUDO_USER)
  -y              Non-interactive — answer yes to all prompts
  --keep-config   Do not remove /etc/zecurity/client.conf
  --keep-state    Do not remove ~/.local/share/zecurity-client (encrypted workspace state)
  -h              Show this help

Typical usage:
  sudo $0              # interactive, full removal
  sudo $0 -y           # full removal, no prompts
  sudo $0 -y --keep-state   # remove binary and service, keep encrypted state
EOF
}

# ── Parse flags ─────────────────────────────────────────────────────────────
while [[ $# -gt 0 ]]; do
    case "$1" in
        -y|--yes)      ASSUME_YES=1 ;;
        -u)            shift; ENROLLING_USER="${1:-}"; [[ -n "$ENROLLING_USER" ]] || err "-u requires a username" ;;
        --keep-config) KEEP_CONFIG=1 ;;
        --keep-state)  KEEP_STATE=1 ;;
        -h|--help)     usage; exit 0 ;;
        *)             usage; exit 1 ;;
    esac
    shift
done

# ── Checks ──────────────────────────────────────────────────────────────────
[[ $EUID -eq 0 ]] || err "must run as root (use sudo)"
command -v systemctl >/dev/null || err "systemctl is required (systemd hosts only)"

[[ -n "$ENROLLING_USER" ]] || err "cannot determine target user — pass -u <user> or run via sudo"
id "$ENROLLING_USER" >/dev/null 2>&1 || err "user '$ENROLLING_USER' does not exist"

# Resolve the state directory for the target user.
# dirs::data_local_dir() in Rust resolves to ~/.local/share on Linux.
USER_HOME="$(getent passwd "$ENROLLING_USER" | cut -d: -f6)"
STATE_DIR="${USER_HOME}/.local/share/zecurity-client"

# ── Summary + confirm ───────────────────────────────────────────────────────
log "about to uninstall the ZECURITY client. actions:"
printf '  - stop + disable %s\n'   "$SERVICE_NAME"
printf '  - remove %s\n'           "${SYSTEMD_DIR}/${SERVICE_NAME}"
printf '  - remove binary %s\n'    "$INSTALL_BIN"
[[ $KEEP_CONFIG -eq 0 ]] \
    && printf '  - remove config %s\n'  "$CONFIG_FILE" \
    || printf '  - KEEP  config %s\n'   "$CONFIG_FILE"
[[ $KEEP_STATE -eq 0 ]] \
    && printf '  - remove state  %s  (encrypted workspace state + keys)\n' "$STATE_DIR" \
    || printf '  - KEEP  state   %s\n' "$STATE_DIR"
printf '\n'

confirm "Proceed with uninstall?" || { log "aborted"; exit 0; }

# ── Stop + disable service ──────────────────────────────────────────────────
if systemctl list-unit-files "$SERVICE_NAME" --no-legend 2>/dev/null | grep -q "$SERVICE_NAME"; then
    log "stopping $SERVICE_NAME"
    systemctl stop "$SERVICE_NAME" 2>/dev/null || true
    log "disabling $SERVICE_NAME"
    systemctl disable "$SERVICE_NAME" 2>/dev/null || true
fi

# ── Remove service file ──────────────────────────────────────────────────────
if [[ -f "${SYSTEMD_DIR}/${SERVICE_NAME}" ]]; then
    log "removing ${SYSTEMD_DIR}/${SERVICE_NAME}"
    rm -f "${SYSTEMD_DIR}/${SERVICE_NAME}"
fi

log "reloading systemd"
systemctl daemon-reload
systemctl reset-failed "$SERVICE_NAME" 2>/dev/null || true

# ── Remove binary ────────────────────────────────────────────────────────────
if [[ -f "$INSTALL_BIN" ]]; then
    log "removing $INSTALL_BIN"
    rm -f "$INSTALL_BIN"
fi

# ── Remove config ────────────────────────────────────────────────────────────
if [[ $KEEP_CONFIG -eq 0 && -f "$CONFIG_FILE" ]]; then
    log "removing $CONFIG_FILE"
    rm -f "$CONFIG_FILE"
fi

# ── Remove user state directory ──────────────────────────────────────────────
# NOTE: this deletes the encrypted workspace state and the per-workspace
# key files. A fresh login will be required after reinstall.
if [[ $KEEP_STATE -eq 0 && -d "$STATE_DIR" ]]; then
    log "removing $STATE_DIR (encrypted workspace state + key files)"
    rm -rf "$STATE_DIR"
fi

# ── Final report ─────────────────────────────────────────────────────────────
log "uninstall complete"

remaining=()
[[ -f "$INSTALL_BIN" ]]  && remaining+=("$INSTALL_BIN")
[[ -f "$CONFIG_FILE" ]]  && remaining+=("$CONFIG_FILE")
[[ -d "$STATE_DIR" ]]    && remaining+=("$STATE_DIR")

if [[ ${#remaining[@]} -gt 0 ]]; then
    log "intentionally preserved:"
    for item in "${remaining[@]}"; do
        printf '  - %s\n' "$item"
    done
fi
