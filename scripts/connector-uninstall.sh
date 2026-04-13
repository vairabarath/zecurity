#!/usr/bin/env bash
# connector-uninstall.sh — ZECURITY connector uninstaller for Linux hosts
#
# Reverses everything connector-install.sh set up:
#   - stops + disables zecurity-connector.service
#   - stops + disables zecurity-connector-update.timer / .service
#   - removes systemd unit files from /etc/systemd/system
#   - removes /usr/local/bin/zecurity-connector
#   - removes /etc/zecurity/ (config + CA cert)
#   - removes /var/lib/zecurity-connector/ (key, cert, state.json)
#   - removes the 'zecurity' system user (with --keep-user to preserve)
#
# Default behavior PROMPTS before destructive actions. Use -y to skip prompts.
#
# Flags:
#   -y              Non-interactive: answer yes to all prompts
#   --keep-user     Preserve the 'zecurity' system user
#   --keep-state    Preserve /var/lib/zecurity-connector (keys + enrollment)
#   --keep-config   Preserve /etc/zecurity (config + ca.crt)
#   -h              Show help
#
# Usage:
#   sudo ./connector-uninstall.sh              # interactive
#   sudo ./connector-uninstall.sh -y           # no prompts
#   sudo ./connector-uninstall.sh -y --keep-state  # leave state dir intact

set -euo pipefail

# ── Defaults ────────────────────────────────────────────────────────────────
SERVICE_USER="zecurity"
INSTALL_BIN="/usr/local/bin/zecurity-connector"
CONFIG_DIR="/etc/zecurity"
STATE_DIR="/var/lib/zecurity-connector"
SYSTEMD_DIR="/etc/systemd/system"

MAIN_SERVICE="zecurity-connector.service"
UPDATE_SERVICE="zecurity-connector-update.service"
UPDATE_TIMER="zecurity-connector-update.timer"

ASSUME_YES=0
KEEP_USER=0
KEEP_STATE=0
KEEP_CONFIG=0

# ── Helpers ─────────────────────────────────────────────────────────────────
log()  { printf '\033[1;34m[uninstall]\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[uninstall]\033[0m %s\n' "$*" >&2; }
err()  { printf '\033[1;31m[uninstall]\033[0m %s\n' "$*" >&2; exit 1; }

confirm() {
    # $1 = prompt
    if [[ $ASSUME_YES -eq 1 ]]; then
        return 0
    fi
    local reply
    read -r -p "$(printf '\033[1;33m[uninstall]\033[0m %s [y/N] ' "$1")" reply
    [[ "$reply" =~ ^[Yy]$ ]]
}

usage() {
    cat <<EOF
Usage: $0 [-y] [--keep-user] [--keep-state] [--keep-config] [-h]

Reverses the ZECURITY connector installation.

Flags:
  -y              Non-interactive — answer yes to all prompts
  --keep-user     Do not remove the 'zecurity' system user
  --keep-state    Do not remove /var/lib/zecurity-connector (keys + enrollment state)
  --keep-config   Do not remove /etc/zecurity (config file + CA cert)
  -h              Show this help

Typical usage:
  sudo $0              # interactive, full removal
  sudo $0 -y           # full removal, no prompts
  sudo $0 -y --keep-state --keep-config   # only remove binary and systemd units
EOF
}

# ── Parse flags ─────────────────────────────────────────────────────────────
while [[ $# -gt 0 ]]; do
    case "$1" in
        -y|--yes)      ASSUME_YES=1 ;;
        --keep-user)   KEEP_USER=1 ;;
        --keep-state)  KEEP_STATE=1 ;;
        --keep-config) KEEP_CONFIG=1 ;;
        -h|--help)     usage; exit 0 ;;
        *)             usage; exit 1 ;;
    esac
    shift
done

# ── Checks ──────────────────────────────────────────────────────────────────
[[ $EUID -eq 0 ]] || err "must run as root (use sudo)"

command -v systemctl >/dev/null || err "systemctl is required (this uninstaller supports systemd hosts only)"

# ── Summary + confirm ───────────────────────────────────────────────────────
log "about to uninstall the ZECURITY connector. actions:"
printf '  - stop + disable %s\n' "$MAIN_SERVICE" "$UPDATE_TIMER" "$UPDATE_SERVICE"
printf '  - remove systemd unit files from %s\n' "$SYSTEMD_DIR"
printf '  - remove binary %s\n' "$INSTALL_BIN"
[[ $KEEP_CONFIG -eq 0 ]] && printf '  - remove config dir %s\n' "$CONFIG_DIR"            || printf '  - KEEP  config dir %s\n' "$CONFIG_DIR"
[[ $KEEP_STATE  -eq 0 ]] && printf '  - remove state dir  %s\n' "$STATE_DIR"             || printf '  - KEEP  state dir  %s\n' "$STATE_DIR"
[[ $KEEP_USER   -eq 0 ]] && printf "  - remove system user '%s'\n" "$SERVICE_USER"        || printf "  - KEEP  system user '%s'\n" "$SERVICE_USER"
echo ""

confirm "Proceed with uninstall?" || { log "aborted"; exit 0; }

# ── Stop + disable services ─────────────────────────────────────────────────
# The 'systemctl is-active' check is cheap and avoids noisy errors if the
# service was already stopped. `|| true` suppresses errors from missing units.
for unit in "$UPDATE_TIMER" "$UPDATE_SERVICE" "$MAIN_SERVICE"; do
    if systemctl list-unit-files "$unit" --no-legend 2>/dev/null | grep -q "$unit"; then
        log "stopping $unit"
        systemctl stop "$unit" 2>/dev/null || true
        log "disabling $unit"
        systemctl disable "$unit" 2>/dev/null || true
    fi
done

# ── Remove systemd unit files ───────────────────────────────────────────────
for unit in "$MAIN_SERVICE" "$UPDATE_SERVICE" "$UPDATE_TIMER"; do
    if [[ -f "${SYSTEMD_DIR}/${unit}" ]]; then
        log "removing ${SYSTEMD_DIR}/${unit}"
        rm -f "${SYSTEMD_DIR}/${unit}"
    fi
done

log "reloading systemd"
systemctl daemon-reload
# reset-failed clears any lingering 'failed' state for our units so future
# installs start from a clean slate.
systemctl reset-failed "$MAIN_SERVICE" "$UPDATE_SERVICE" "$UPDATE_TIMER" 2>/dev/null || true

# ── Remove binary ───────────────────────────────────────────────────────────
if [[ -f "$INSTALL_BIN" ]]; then
    log "removing $INSTALL_BIN"
    rm -f "$INSTALL_BIN"
fi

# ── Remove config directory ─────────────────────────────────────────────────
if [[ $KEEP_CONFIG -eq 0 && -d "$CONFIG_DIR" ]]; then
    log "removing $CONFIG_DIR"
    rm -rf "$CONFIG_DIR"
fi

# ── Remove state directory ──────────────────────────────────────────────────
# NOTE: this deletes the connector's private key. A fresh install will need
# a NEW enrollment token because the cert for the old connector_id is gone.
if [[ $KEEP_STATE -eq 0 && -d "$STATE_DIR" ]]; then
    log "removing $STATE_DIR (private key + cert + state.json)"
    rm -rf "$STATE_DIR"
fi

# ── Remove system user ──────────────────────────────────────────────────────
if [[ $KEEP_USER -eq 0 ]] && id "$SERVICE_USER" >/dev/null 2>&1; then
    log "removing system user: $SERVICE_USER"
    # userdel will fail if the user owns processes. Those should already be
    # killed by the systemctl stop above, but guard anyway.
    if ! userdel "$SERVICE_USER" 2>&1 | tee /tmp/userdel-err.log; then
        warn "userdel failed; check /tmp/userdel-err.log"
    fi
fi

# ── Final report ────────────────────────────────────────────────────────────
log "uninstall complete"

remaining=()
[[ -f "$INSTALL_BIN" ]] && remaining+=("$INSTALL_BIN")
[[ -d "$CONFIG_DIR" ]] && remaining+=("$CONFIG_DIR")
[[ -d "$STATE_DIR" ]] && remaining+=("$STATE_DIR")
id "$SERVICE_USER" >/dev/null 2>&1 && remaining+=("user:$SERVICE_USER")

if [[ ${#remaining[@]} -gt 0 ]]; then
    log "intentionally preserved:"
    for item in "${remaining[@]}"; do
        printf '  - %s\n' "$item"
    done
fi
