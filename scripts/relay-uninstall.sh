#!/usr/bin/env bash
# relay-uninstall.sh — ZECURITY relay uninstaller for Linux hosts
#
# Reverses everything relay-local-install.sh set up:
#   - stops + disables zecurity-relay.service
#   - removes the systemd unit file from /etc/systemd/system
#   - removes /usr/local/bin/zecurity-relay
#   - removes /etc/zecurity/relay.conf   (config dir kept if other components remain)
#   - removes /var/lib/zecurity-relay/   (private key + cert + intermediate CA)
#   - removes the 'zecurity' system user (skipped if any other component remains)
#
# Default behavior PROMPTS before destructive actions. Use -y to skip prompts.
#
# Flags:
#   -y              Non-interactive: answer yes to all prompts
#   --keep-user     Preserve the 'zecurity' system user
#   --keep-state    Preserve /var/lib/zecurity-relay (key + cert + intermediate CA)
#   --keep-config   Preserve /etc/zecurity/relay.conf
#   -h              Show help

set -euo pipefail

# ── Defaults ────────────────────────────────────────────────────────────────
SERVICE_USER="zecurity"
INSTALL_BIN="/usr/local/bin/zecurity-relay"
SHIELD_BIN="/usr/local/bin/zecurity-shield"
CONNECTOR_BIN="/usr/local/bin/zecurity-connector"
CLIENT_BIN="/usr/local/bin/zecurity-client"
CONFIG_DIR="/etc/zecurity"
CONFIG_FILE="${CONFIG_DIR}/relay.conf"
STATE_DIR="/var/lib/zecurity-relay"
SYSTEMD_DIR="/etc/systemd/system"

MAIN_SERVICE="zecurity-relay.service"

ASSUME_YES=0
KEEP_USER=0
KEEP_STATE=0
KEEP_CONFIG=0

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
Usage: $0 [-y] [--keep-user] [--keep-state] [--keep-config] [-h]

Reverses the ZECURITY relay installation.

Flags:
  -y              Non-interactive — answer yes to all prompts
  --keep-user     Do not remove the 'zecurity' system user
  --keep-state    Do not remove /var/lib/zecurity-relay (private key + cert)
  --keep-config   Do not remove /etc/zecurity/relay.conf
  -h              Show this help

Typical usage:
  sudo $0              # interactive, full removal
  sudo $0 -y           # full removal, no prompts
  sudo $0 -y --keep-state --keep-config   # only remove binary and systemd unit
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

# ── Co-install detection ────────────────────────────────────────────────────
# The 'zecurity' user and /etc/zecurity/ are shared across shield, connector,
# client, and relay. If any sibling is still installed, preserve the user
# automatically and only remove relay.conf (not the whole config dir).
OTHER_INSTALLED=0
remaining_sibs=()
for bin in "$SHIELD_BIN" "$CONNECTOR_BIN" "$CLIENT_BIN"; do
    if [[ -f "$bin" ]]; then
        OTHER_INSTALLED=1
        remaining_sibs+=("$bin")
    fi
done
if [[ $OTHER_INSTALLED -eq 1 ]]; then
    warn "sibling components still installed: ${remaining_sibs[*]}"
    warn "the 'zecurity' user and $CONFIG_DIR will be preserved"
    KEEP_USER=1
fi

# ── Summary + confirm ───────────────────────────────────────────────────────
log "about to uninstall the ZECURITY relay. actions:"
printf '  - stop + disable %s\n' "$MAIN_SERVICE"
printf '  - remove systemd unit %s\n' "${SYSTEMD_DIR}/${MAIN_SERVICE}"
printf '  - remove binary %s\n' "$INSTALL_BIN"

if [[ $KEEP_CONFIG -eq 0 ]]; then
    printf '  - remove config file %s\n' "$CONFIG_FILE"
    if [[ $OTHER_INSTALLED -eq 0 ]]; then
        printf '  - remove config dir %s (if empty)\n' "$CONFIG_DIR"
    fi
else
    printf '  - KEEP  config file %s\n' "$CONFIG_FILE"
fi

[[ $KEEP_STATE -eq 0 ]] && printf '  - remove state dir %s\n' "$STATE_DIR" \
                        || printf '  - KEEP  state dir %s\n' "$STATE_DIR"

[[ $KEEP_USER -eq 0 ]] && printf "  - remove system user '%s'\n" "$SERVICE_USER" \
                       || printf "  - KEEP  system user '%s'\n"  "$SERVICE_USER"
echo ""

confirm "Proceed with uninstall?" || { log "aborted"; exit 0; }

# ── Stop + disable service ──────────────────────────────────────────────────
if systemctl list-unit-files "$MAIN_SERVICE" --no-legend 2>/dev/null | grep -q "$MAIN_SERVICE"; then
    log "stopping $MAIN_SERVICE"
    systemctl stop "$MAIN_SERVICE" 2>/dev/null || true
    log "disabling $MAIN_SERVICE"
    systemctl disable "$MAIN_SERVICE" 2>/dev/null || true
fi

# ── Remove systemd unit ─────────────────────────────────────────────────────
if [[ -f "${SYSTEMD_DIR}/${MAIN_SERVICE}" ]]; then
    log "removing ${SYSTEMD_DIR}/${MAIN_SERVICE}"
    rm -f "${SYSTEMD_DIR}/${MAIN_SERVICE}"
fi

log "reloading systemd"
systemctl daemon-reload
systemctl reset-failed "$MAIN_SERVICE" 2>/dev/null || true

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
    if [[ $OTHER_INSTALLED -eq 0 && -d "$CONFIG_DIR" ]]; then
        if [[ -z "$(ls -A "$CONFIG_DIR" 2>/dev/null)" ]]; then
            log "removing empty $CONFIG_DIR"
            rmdir "$CONFIG_DIR"
        else
            warn "$CONFIG_DIR is not empty — leaving it (remaining: $(ls "$CONFIG_DIR" | tr '\n' ' '))"
        fi
    fi
fi

# ── Remove state directory ──────────────────────────────────────────────────
# NOTE: this deletes the relay's private key. A fresh install will need a
# NEW provisioning token because the cert for the old RELAY_ID is gone.
if [[ $KEEP_STATE -eq 0 && -d "$STATE_DIR" ]]; then
    log "removing $STATE_DIR (private key + cert + intermediate CA)"
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

if [[ ${#remaining[@]} -gt 0 ]]; then
    log "intentionally preserved:"
    for item in "${remaining[@]}"; do
        printf '  - %s\n' "$item"
    done
fi
