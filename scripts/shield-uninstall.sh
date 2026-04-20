#!/usr/bin/env bash
# shield-uninstall.sh — ZECURITY shield uninstaller for Linux hosts
#
# Reverses everything shield-install.sh set up:
#   - stops + disables zecurity-shield.service
#   - stops + disables zecurity-shield-update.timer / .service
#   - removes systemd unit files from /etc/systemd/system
#   - removes /usr/local/bin/zecurity-shield
#   - removes /etc/zecurity/shield.conf  (ca.crt kept if connector still installed)
#   - removes /var/lib/zecurity-shield/  (key, cert, state.json)
#   - flushes the 'inet zecurity' nftables table
#   - removes the zecurity0 TUN interface (if present)
#   - removes the 'zecurity' system user  (skipped if connector is still installed)
#
# Default behavior PROMPTS before destructive actions. Use -y to skip prompts.
#
# Flags:
#   -y              Non-interactive: answer yes to all prompts
#   --keep-user     Preserve the 'zecurity' system user
#   --keep-state    Preserve /var/lib/zecurity-shield (keys + enrollment)
#   --keep-config   Preserve /etc/zecurity/shield.conf
#   --keep-nft      Skip nftables table flush
#   -h              Show help
#
# Usage:
#   sudo ./shield-uninstall.sh              # interactive
#   sudo ./shield-uninstall.sh -y           # no prompts
#   sudo ./shield-uninstall.sh -y --keep-state  # leave state dir intact

set -euo pipefail

# ── Defaults ────────────────────────────────────────────────────────────────
SERVICE_USER="zecurity"
INSTALL_BIN="/usr/local/bin/zecurity-shield"
CONNECTOR_BIN="/usr/local/bin/zecurity-connector"
CONFIG_DIR="/etc/zecurity"
CONFIG_FILE="${CONFIG_DIR}/shield.conf"
STATE_DIR="/var/lib/zecurity-shield"
SYSTEMD_DIR="/etc/systemd/system"
TUN_IFACE="zecurity0"
NFT_TABLE="inet zecurity"

MAIN_SERVICE="zecurity-shield.service"
UPDATE_SERVICE="zecurity-shield-update.service"
UPDATE_TIMER="zecurity-shield-update.timer"

ASSUME_YES=0
KEEP_USER=0
KEEP_STATE=0
KEEP_CONFIG=0
KEEP_NFT=0

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
Usage: $0 [-y] [--keep-user] [--keep-state] [--keep-config] [--keep-nft] [-h]

Reverses the ZECURITY shield installation.

Flags:
  -y              Non-interactive — answer yes to all prompts
  --keep-user     Do not remove the 'zecurity' system user
  --keep-state    Do not remove /var/lib/zecurity-shield (keys + enrollment state)
  --keep-config   Do not remove /etc/zecurity/shield.conf
  --keep-nft      Skip flushing the 'inet zecurity' nftables table
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
        --keep-nft)    KEEP_NFT=1 ;;
        -h|--help)     usage; exit 0 ;;
        *)             usage; exit 1 ;;
    esac
    shift
done

# ── Checks ──────────────────────────────────────────────────────────────────
[[ $EUID -eq 0 ]] || err "must run as root (use sudo)"

command -v systemctl >/dev/null || err "systemctl is required (this uninstaller supports systemd hosts only)"

# ── Connector co-install detection ─────────────────────────────────────────
# The 'zecurity' user and /etc/zecurity/ are shared with the connector.
# If the connector binary is still present, preserve the user automatically
# and only remove the shield-specific config file (not the whole dir).
CONNECTOR_INSTALLED=0
if [[ -f "$CONNECTOR_BIN" ]]; then
    CONNECTOR_INSTALLED=1
    warn "connector binary detected at $CONNECTOR_BIN"
    warn "the 'zecurity' user and $CONFIG_DIR will be preserved (connector still installed)"
    KEEP_USER=1
fi

# ── Summary + confirm ───────────────────────────────────────────────────────
log "about to uninstall the ZECURITY shield. actions:"
printf '  - stop + disable %s\n' "$MAIN_SERVICE" "$UPDATE_TIMER" "$UPDATE_SERVICE"
printf '  - remove systemd unit files from %s\n' "$SYSTEMD_DIR"
printf '  - remove binary %s\n' "$INSTALL_BIN"

if [[ $KEEP_CONFIG -eq 0 ]]; then
    printf '  - remove config file %s\n' "$CONFIG_FILE"
    if [[ $CONNECTOR_INSTALLED -eq 0 ]]; then
        printf '  - remove config dir %s (empty after shield.conf removed)\n' "$CONFIG_DIR"
    fi
else
    printf '  - KEEP  config file %s\n' "$CONFIG_FILE"
fi

[[ $KEEP_STATE  -eq 0 ]] && printf '  - remove state dir  %s\n' "$STATE_DIR" \
                          || printf '  - KEEP  state dir  %s\n'  "$STATE_DIR"

[[ $KEEP_NFT    -eq 0 ]] && printf '  - flush nftables table: %s\n' "$NFT_TABLE" \
                          || printf '  - KEEP  nftables table: %s\n' "$NFT_TABLE"

printf '  - remove TUN interface %s (if present)\n' "$TUN_IFACE"

[[ $KEEP_USER   -eq 0 ]] && printf "  - remove system user '%s'\n" "$SERVICE_USER" \
                          || printf "  - KEEP  system user '%s'\n"  "$SERVICE_USER"
echo ""

confirm "Proceed with uninstall?" || { log "aborted"; exit 0; }

# ── Stop + disable services ─────────────────────────────────────────────────
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
systemctl reset-failed "$MAIN_SERVICE" "$UPDATE_SERVICE" "$UPDATE_TIMER" 2>/dev/null || true

# ── Flush nftables table ────────────────────────────────────────────────────
if [[ $KEEP_NFT -eq 0 ]]; then
    if command -v nft >/dev/null 2>&1; then
        if nft list table $NFT_TABLE >/dev/null 2>&1; then
            log "flushing nftables table: $NFT_TABLE"
            nft delete table $NFT_TABLE 2>/dev/null || warn "failed to delete nftables table $NFT_TABLE (may already be gone)"
        else
            log "nftables table '$NFT_TABLE' not present — skipping"
        fi
    else
        warn "nft not found — skipping nftables cleanup"
    fi
fi

# ── Remove TUN interface ────────────────────────────────────────────────────
if ip link show "$TUN_IFACE" >/dev/null 2>&1; then
    log "removing TUN interface $TUN_IFACE"
    ip link delete "$TUN_IFACE" 2>/dev/null || warn "failed to remove $TUN_IFACE (may already be gone)"
else
    log "TUN interface $TUN_IFACE not present — skipping"
fi

# ── Remove binary ───────────────────────────────────────────────────────────
if [[ -f "$INSTALL_BIN" ]]; then
    log "removing $INSTALL_BIN"
    rm -f "$INSTALL_BIN"
fi

# ── Remove config ───────────────────────────────────────────────────────────
if [[ $KEEP_CONFIG -eq 0 ]]; then
    if [[ -f "$CONFIG_FILE" ]]; then
        log "removing $CONFIG_FILE"
        rm -f "$CONFIG_FILE"
    fi
    # Only remove the directory if the connector isn't sharing it.
    if [[ $CONNECTOR_INSTALLED -eq 0 && -d "$CONFIG_DIR" ]]; then
        # Remove dir only if empty (ca.crt may have been cleaned by connector uninstall).
        if [[ -z "$(ls -A "$CONFIG_DIR" 2>/dev/null)" ]]; then
            log "removing empty $CONFIG_DIR"
            rmdir "$CONFIG_DIR"
        else
            warn "$CONFIG_DIR is not empty — leaving it (remaining files: $(ls "$CONFIG_DIR" | tr '\n' ' '))"
        fi
    fi
fi

# ── Remove state directory ──────────────────────────────────────────────────
# NOTE: this deletes the shield's private key. A fresh install will need
# a NEW enrollment token because the cert for the old shield_id is gone.
if [[ $KEEP_STATE -eq 0 && -d "$STATE_DIR" ]]; then
    log "removing $STATE_DIR (private key + cert + state.json)"
    rm -rf "$STATE_DIR"
fi

# ── Remove system user ──────────────────────────────────────────────────────
if [[ $KEEP_USER -eq 0 ]] && id "$SERVICE_USER" >/dev/null 2>&1; then
    log "removing system user: $SERVICE_USER"
    if ! userdel "$SERVICE_USER" 2>&1 | tee /tmp/userdel-err.log; then
        warn "userdel failed; check /tmp/userdel-err.log"
    fi
fi

# ── Final report ────────────────────────────────────────────────────────────
log "uninstall complete"

remaining=()
[[ -f "$INSTALL_BIN" ]] && remaining+=("$INSTALL_BIN")
[[ -f "$CONFIG_FILE" ]] && remaining+=("$CONFIG_FILE")
[[ -d "$STATE_DIR"   ]] && remaining+=("$STATE_DIR")
id "$SERVICE_USER" >/dev/null 2>&1 && remaining+=("user:$SERVICE_USER")
nft list table $NFT_TABLE >/dev/null 2>&1 && remaining+=("nftables:$NFT_TABLE")
ip link show "$TUN_IFACE" >/dev/null 2>&1 && remaining+=("iface:$TUN_IFACE")

if [[ ${#remaining[@]} -gt 0 ]]; then
    log "intentionally preserved:"
    for item in "${remaining[@]}"; do
        printf '  - %s\n' "$item"
    done
fi
