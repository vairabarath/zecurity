#!/usr/bin/env bash
# relay-local-install.sh — ZECURITY relay local installer
#
# Installs:
#   - zecurity system user (shared with shield/connector/client)
#   - /usr/local/bin/zecurity-relay                  (from local binary)
#   - /etc/zecurity/relay.conf                       (config)
#   - /var/lib/zecurity-relay/pki/                   (state dir; relay binary
#                                                     writes relay.key,
#                                                     relay.crt, intermediate-ca.crt
#                                                     here on first boot)
#   - /etc/systemd/system/zecurity-relay.service
#
# Required environment variables:
#   CONTROLLER_ADDR        — controller gRPC address, e.g. "localhost:9090"
#   CONTROLLER_HTTP_ADDR   — controller HTTP address, e.g. "localhost:8080"
#
# Optional environment variables (auto-derived when omitted):
#   RELAY_ID               — canonical lowercase UUID; must match the row
#                            created by POST /api/relays on the controller.
#                            If unset: generated once, persisted under the
#                            state dir at ${STATE_DIR}/relay_id, and reused
#                            on subsequent installs.
#   RELAY_CA_FINGERPRINT   — 64-char SHA-256 hex of the controller
#                            intermediate CA (operator pre-pins; relay
#                            verifies after fetching /ca.crt). If unset:
#                            derived automatically from
#                            http://${CONTROLLER_HTTP_ADDR}/ca.crt.
#
# Optional environment variables (forwarded into relay.conf if set):
#   RELAY_BIND                       (default 0.0.0.0:9093)
#   RELAY_DNS_SANS                   (comma-separated, e.g. relay.example.com)
#   RELAY_IP_SANS                    (comma-separated, e.g. 10.0.0.50)
#   RELAY_STATE_DIR                  (default /var/lib/zecurity-relay/pki)
#   RELAY_PROVISIONING_TOKEN         (forward-compat; ignored until controller
#                                     enforces token auth on Provision)
#   LOG_LEVEL                        (default info)
#   RELAY_MAX_CONNECTIONS
#   RELAY_MAX_LOOKUP_BRIDGES
#   RELAY_MAX_BIDI_STREAMS
#   RELAY_IDLE_TIMEOUT_SECS
#   RELAY_HANDSHAKE_TIMEOUT_SECS
#   RELAY_MESSAGE_TIMEOUT_SECS
#   RELAY_HEARTBEAT_INTERVAL_SECS

set -euo pipefail

# ── Defaults ────────────────────────────────────────────────────────────────
SERVICE_USER="zecurity"
INSTALL_BIN="/usr/local/bin/zecurity-relay"
CONFIG_DIR="/etc/zecurity"
CONFIG_FILE="${CONFIG_DIR}/relay.conf"
STATE_DIR="/var/lib/zecurity-relay"
PKI_DIR="${STATE_DIR}/pki"
SYSTEMD_DIR="/etc/systemd/system"

# ── Helpers ─────────────────────────────────────────────────────────────────
log()  { printf '\033[1;34m[install]\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[install]\033[0m %s\n' "$*" >&2; }
err()  { printf '\033[1;31m[install]\033[0m %s\n' "$*" >&2; exit 1; }

usage() {
    cat <<EOF
Usage: $0 <path-to-binary>

Installs the ZECURITY relay locally.

Required env vars:
  CONTROLLER_ADDR        gRPC host:port for provisioning + heartbeat (e.g. localhost:9090)
  CONTROLLER_HTTP_ADDR   HTTP host:port for /ca.crt fetch              (e.g. localhost:8080)

Auto-derived if not set (override only when the controller enforces them):
  RELAY_ID               Canonical lowercase UUID. Generated + persisted at
                         \${STATE_DIR}/relay_id on first run; reused after.
  RELAY_CA_FINGERPRINT   64-char SHA-256 hex of the controller intermediate CA.
                         Computed from http://\${CONTROLLER_HTTP_ADDR}/ca.crt.

Optional env vars (passed through to /etc/zecurity/relay.conf if set):
  RELAY_BIND, RELAY_DNS_SANS, RELAY_IP_SANS, RELAY_STATE_DIR, LOG_LEVEL,
  RELAY_PROVISIONING_TOKEN,
  RELAY_MAX_CONNECTIONS, RELAY_MAX_LOOKUP_BRIDGES, RELAY_MAX_BIDI_STREAMS,
  RELAY_IDLE_TIMEOUT_SECS, RELAY_HANDSHAKE_TIMEOUT_SECS,
  RELAY_MESSAGE_TIMEOUT_SECS, RELAY_HEARTBEAT_INTERVAL_SECS
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

command -v systemctl >/dev/null || err "systemctl is required"

# ── Auto-derive RELAY_ID / RELAY_CA_FINGERPRINT if the operator didn't supply
#    them. Matches the persist-and-reuse pattern of run-relay-local.sh.
ID_FILE="${STATE_DIR}/relay_id"

if [[ -z "${RELAY_ID:-}" ]]; then
    if [[ -f "$ID_FILE" ]]; then
        RELAY_ID="$(cat "$ID_FILE")"
        log "RELAY_ID reused from $ID_FILE = $RELAY_ID"
    else
        command -v uuidgen >/dev/null || [[ -r /proc/sys/kernel/random/uuid ]] \
            || err "cannot auto-generate RELAY_ID (need uuidgen or /proc/sys/kernel/random/uuid)"
        if [[ -r /proc/sys/kernel/random/uuid ]]; then
            RELAY_ID="$(cat /proc/sys/kernel/random/uuid)"
        else
            RELAY_ID="$(uuidgen | tr 'A-Z' 'a-z')"
        fi
        log "RELAY_ID generated = $RELAY_ID"
    fi
fi

if [[ -z "${RELAY_CA_FINGERPRINT:-}" ]]; then
    command -v openssl  >/dev/null || err "openssl is required to derive RELAY_CA_FINGERPRINT"
    command -v curl     >/dev/null || err "curl is required to fetch /ca.crt"
    command -v sha256sum >/dev/null || err "sha256sum is required to derive RELAY_CA_FINGERPRINT"
    log "fetching CA fingerprint from http://${CONTROLLER_HTTP_ADDR}/ca.crt"
    RELAY_CA_FINGERPRINT="$(
        curl -fsS "http://${CONTROLLER_HTTP_ADDR}/ca.crt" \
            | openssl x509 -outform DER 2>/dev/null \
            | sha256sum | awk '{print $1}'
    )" || err "could not reach the controller at ${CONTROLLER_HTTP_ADDR}. Is it running?"
    log "RELAY_CA_FINGERPRINT = $RELAY_CA_FINGERPRINT"
fi

# Light sanity checks — apply to both operator-supplied and auto-derived values.
if [[ ! "$RELAY_ID" =~ ^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$ ]]; then
    err "RELAY_ID must be a canonical lowercase UUID (got: $RELAY_ID)"
fi
if [[ ! "$RELAY_CA_FINGERPRINT" =~ ^[0-9a-fA-F]{64}$ ]]; then
    err "RELAY_CA_FINGERPRINT must be a 64-character SHA-256 hex digest"
fi

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
install -d -m 0700 -o "$SERVICE_USER" -g "$SERVICE_USER" "$PKI_DIR"

# Persist RELAY_ID so a subsequent reinstall (without env override) reuses
# it — the on-disk cert is bound to this UUID.
if [[ ! -f "$ID_FILE" ]]; then
    printf '%s\n' "$RELAY_ID" | install -m 0600 -o "$SERVICE_USER" -g "$SERVICE_USER" /dev/stdin "$ID_FILE"
fi

# ── Install binary ──────────────────────────────────────────────────────────
log "installing binary to $INSTALL_BIN"
install -m 0755 -o root -g root "$LOCAL_BINARY" "$INSTALL_BIN"

# ── Install systemd unit ────────────────────────────────────────────────────
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
UNITS_SRC="${REPO_ROOT}/relay/systemd"

if [[ ! -d "$UNITS_SRC" ]]; then
    err "systemd units not found in $UNITS_SRC"
fi

log "installing systemd unit from $UNITS_SRC"
install -m 0644 "${UNITS_SRC}/zecurity-relay.service" "${SYSTEMD_DIR}/"

# ── Write config file ───────────────────────────────────────────────────────
log "writing $CONFIG_FILE"
{
    echo "# ZECURITY relay configuration — written by relay-local-install.sh"
    echo "CONTROLLER_ADDR=${CONTROLLER_ADDR}"
    echo "CONTROLLER_HTTP_ADDR=${CONTROLLER_HTTP_ADDR}"
    echo "RELAY_ID=${RELAY_ID}"
    echo "RELAY_CA_FINGERPRINT=${RELAY_CA_FINGERPRINT}"
    echo "RELAY_STATE_DIR=${RELAY_STATE_DIR:-$PKI_DIR}"
    echo "LOG_LEVEL=${LOG_LEVEL:-info}"

    # Optional pass-throughs — only emitted if the operator set them.
    for var in RELAY_BIND RELAY_DNS_SANS RELAY_IP_SANS \
               RELAY_PROVISIONING_TOKEN \
               RELAY_MAX_CONNECTIONS RELAY_MAX_LOOKUP_BRIDGES \
               RELAY_MAX_BIDI_STREAMS RELAY_IDLE_TIMEOUT_SECS \
               RELAY_HANDSHAKE_TIMEOUT_SECS RELAY_MESSAGE_TIMEOUT_SECS \
               RELAY_HEARTBEAT_INTERVAL_SECS; do
        if [[ -n "${!var:-}" ]]; then
            echo "${var}=${!var}"
        fi
    done
} > "$CONFIG_FILE"

chmod 0660 "$CONFIG_FILE"
chown root:"$SERVICE_USER" "$CONFIG_FILE"

# ── Reload systemd + enable + start ─────────────────────────────────────────
log "reloading systemd"
systemctl daemon-reload
systemctl enable --now zecurity-relay.service

log "install complete"
systemctl status zecurity-relay.service --no-pager --lines=5 || true
