#!/usr/bin/env bash
# dns-helper-install.sh — install the privileged DNS helper (ADR-023 option C).
#
# ⚠️ OPT-IN ON PURPOSE. This installs a **root** systemd service. The client works
# without it — managed names stay reachable by synthetic IP or a hosts entry, and the
# daemon fails soft if the helper is absent. Adding a root service to every client
# install would raise the default privilege of the product for a convenience, so it is
# a deliberate, separate step.
#
# What it does:
#   1. creates the `zecurity` group (socket group; defence in depth only)
#   2. adds the client daemon's user to it
#   3. installs /usr/local/bin/zecurity-dns-helper
#   4. substitutes @ALLOW_UID@ and installs both units
#   5. enables the .socket (socket activation — root runs only during a change)
#
# Usage:
#   sudo ./dns-helper-install.sh                 # infer the user from the client unit
#   sudo ./dns-helper-install.sh --user <name>   # or say it explicitly
#   sudo ./dns-helper-install.sh --uninstall

set -euo pipefail

INSTALL_BIN="/usr/local/bin/zecurity-dns-helper"
SYSTEMD_DIR="/etc/systemd/system"
GROUP="zecurity"
CLIENT_UNIT="${SYSTEMD_DIR}/zecurity-client.service"
SRC_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

log()  { printf '\033[1;34m[dns-helper]\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[dns-helper]\033[0m %s\n' "$*"; }
err()  { printf '\033[1;31m[dns-helper]\033[0m %s\n' "$*" >&2; exit 1; }

# Parse arguments BEFORE the privilege check, so `--help` and a bad argument are
# answered without demanding sudo first.
MODE=install
DAEMON_USER=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --user)      DAEMON_USER="${2:-}"; shift 2 ;;
    --uninstall) MODE=uninstall; shift ;;
    -h|--help)   sed -n '2,22p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *)           err "unknown argument: $1" ;;
  esac
done

[[ $EUID -eq 0 ]] || err "must run as root (installs a root systemd service)"

# ── uninstall ───────────────────────────────────────────────────────────────
if [[ "$MODE" == uninstall ]]; then
  log "removing the DNS helper"
  systemctl disable --now zecurity-dns-helper.socket 2>/dev/null || true
  systemctl stop zecurity-dns-helper.service 2>/dev/null || true
  rm -f "${SYSTEMD_DIR}/zecurity-dns-helper.socket" \
        "${SYSTEMD_DIR}/zecurity-dns-helper.service" \
        "$INSTALL_BIN" /run/zecurity-dns-helper.sock
  systemctl daemon-reload
  # The group is left in place: other things may reference it, and removing a group
  # that still appears in a user's membership list is a worse outcome than a stray one.
  log "removed. The '${GROUP}' group was left alone."
  warn "managed names will no longer resolve automatically — use a hosts entry"
  exit 0
fi

# ── 0. preconditions ────────────────────────────────────────────────────────
# ADR-023 makes systemd-resolved a precondition: it is the only backend the helper
# knows, and without it there is nothing to configure.
if ! systemctl is-active --quiet systemd-resolved; then
  err "systemd-resolved is not active — the helper has no backend to configure.
     Managed names still work via a hosts entry; see 'zecurity-client resources'."
fi
command -v resolvectl >/dev/null || err "resolvectl not found (systemd-resolved tooling missing)"

# ── 1. who is the daemon? ───────────────────────────────────────────────────
if [[ -z "$DAEMON_USER" ]]; then
  [[ -f "$CLIENT_UNIT" ]] || err "cannot find ${CLIENT_UNIT}; pass --user <name> explicitly"
  DAEMON_USER="$(sed -n 's/^User=\(.*\)$/\1/p' "$CLIENT_UNIT" | head -1)"
  [[ -n "$DAEMON_USER" ]] || err "User= is empty in ${CLIENT_UNIT}; pass --user <name> explicitly"
  log "inferred daemon user from the client unit: ${DAEMON_USER}"
fi
id -u "$DAEMON_USER" >/dev/null 2>&1 || err "user ${DAEMON_USER} does not exist"
ALLOW_UID="$(id -u "$DAEMON_USER")"
[[ "$ALLOW_UID" != "0" ]] || warn "daemon user is root; --allow-uid will be 0"
log "helper will accept requests from uid ${ALLOW_UID} (${DAEMON_USER}) and root only"

# ── 2. group ────────────────────────────────────────────────────────────────
if getent group "$GROUP" >/dev/null; then
  log "group ${GROUP} already exists"
else
  groupadd --system "$GROUP"
  log "created group ${GROUP}"
fi
if id -nG "$DAEMON_USER" | tr ' ' '\n' | grep -qx "$GROUP"; then
  log "${DAEMON_USER} is already in ${GROUP}"
else
  usermod -aG "$GROUP" "$DAEMON_USER"
  log "added ${DAEMON_USER} to ${GROUP}"
  warn "group membership is picked up by NEW processes only — the client daemon is"
  warn "restarted below so it takes effect without a logout."
fi

# ── 3. binary ───────────────────────────────────────────────────────────────
BIN_SRC=""
for cand in "${SRC_DIR}/target/release/zecurity-dns-helper" \
            "${SRC_DIR}/target/debug/zecurity-dns-helper"; do
  [[ -x "$cand" ]] && { BIN_SRC="$cand"; break; }
done
[[ -n "$BIN_SRC" ]] || err "no built helper found. Run:
     cargo build --release --manifest-path ${SRC_DIR}/Cargo.toml"
install -m 0755 -o root -g root "$BIN_SRC" "$INSTALL_BIN"
log "installed $(basename "$BIN_SRC") -> ${INSTALL_BIN}"

# ── 4. units ────────────────────────────────────────────────────────────────
UNITS_SRC="${SRC_DIR}/systemd"
[[ -d "$UNITS_SRC" ]] || err "unit files not found in ${UNITS_SRC}"
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
sed "s/@ALLOW_UID@/${ALLOW_UID}/" "${UNITS_SRC}/zecurity-dns-helper.service" > "${TMP}/svc"
grep -q '@ALLOW_UID@' "${TMP}/svc" && err "failed to substitute @ALLOW_UID@"
grep -q -- "--allow-uid ${ALLOW_UID}" "${TMP}/svc" || err "substitution produced no --allow-uid"
install -m 0644 "${TMP}/svc" "${SYSTEMD_DIR}/zecurity-dns-helper.service"
install -m 0644 "${UNITS_SRC}/zecurity-dns-helper.socket" "${SYSTEMD_DIR}/zecurity-dns-helper.socket"
log "installed both units (--allow-uid ${ALLOW_UID})"

# ── 5. enable ───────────────────────────────────────────────────────────────
systemctl daemon-reload
systemctl enable --now zecurity-dns-helper.socket
log "enabled zecurity-dns-helper.socket"

# The daemon must be restarted to pick up its new group membership before it can
# reach the socket. Without this the first apply fails with EACCES and looks like a
# helper bug.
if systemctl is-enabled --quiet zecurity-client.service 2>/dev/null; then
  systemctl restart zecurity-client.service
  log "restarted zecurity-client so it picks up the ${GROUP} group"
fi

# ── verify ──────────────────────────────────────────────────────────────────
echo ""
log "verifying"
ls -l /run/zecurity-dns-helper.sock 2>/dev/null | sed 's/^/  /' \
  || warn "socket not present yet (it appears on first use with Accept=no)"
systemctl is-active zecurity-dns-helper.socket >/dev/null \
  && log "  socket unit: active" || warn "  socket unit is NOT active"
echo ""
log "done. Managed names should now resolve without a hosts entry:"
log "  zecurity-client up && dig <managed.name>"
log ""
log "if a name does not resolve, in order:"
log "  journalctl -u zecurity-dns-helper -n 30"
log "  journalctl -u zecurity-client -n 30 | grep -i dns"
log "  resolvectl status zecurity0"
