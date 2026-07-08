#!/usr/bin/env bash
# run-relay-local.sh — run a Zecurity relay locally in the foreground for
# real-time testing (NOT systemd; that's relay-local-install.sh).
#
# It builds the relay, auto-derives the controller CA fingerprint from
# /ca.crt, generates+persists a RELAY_ID per instance, points the relay at a
# local writable state dir, and execs it with debug logging.
#
# Prereqs: the controller must already be running (gRPC :9090, HTTP :8080).
#   cd controller && docker compose up -d        # Postgres + Valkey/Redis
#   cd controller && go run ./cmd/server          # controller itself
#
# Usage:
#   scripts/run-relay-local.sh [instance-name] [bind-port]
#   scripts/run-relay-local.sh relay1 9093        # first relay  (default)
#   scripts/run-relay-local.sh relay2 9094        # second relay (for migration/drain tests)
#
# Override anything via env, e.g.:
#   CONTROLLER_ADDR=localhost:9090 CONTROLLER_HTTP_ADDR=localhost:8080 \
#   RELAY_MAX_CONNECTIONS=4 RELAY_HEARTBEAT_INTERVAL_SECS=10 \
#   scripts/run-relay-local.sh relay1 9093

set -euo pipefail

INSTANCE="${1:-relay1}"
PORT="${2:-9093}"

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DEV_DIR="${REPO_ROOT}/.localdev/${INSTANCE}"
STATE_DIR="${DEV_DIR}/pki"
ID_FILE="${DEV_DIR}/relay_id"

CONTROLLER_ADDR="${CONTROLLER_ADDR:-localhost:9090}"
CONTROLLER_HTTP_ADDR="${CONTROLLER_HTTP_ADDR:-localhost:8080}"

log() { printf '\033[1;34m[run-relay:%s]\033[0m %s\n' "$INSTANCE" "$*"; }
err() { printf '\033[1;31m[run-relay:%s]\033[0m %s\n' "$INSTANCE" "$*" >&2; exit 1; }

command -v openssl >/dev/null || err "openssl is required to compute the CA fingerprint"
command -v curl    >/dev/null || err "curl is required to fetch /ca.crt"

mkdir -p "$STATE_DIR"

# 1. RELAY_ID — generate once per instance and reuse (the state dir holds the
#    cert that is bound to this id, so it must stay stable across restarts).
if [[ -n "${RELAY_ID:-}" ]]; then
    echo "$RELAY_ID" > "$ID_FILE"
elif [[ -f "$ID_FILE" ]]; then
    RELAY_ID="$(cat "$ID_FILE")"
else
    RELAY_ID="$(cat /proc/sys/kernel/random/uuid)"   # always lowercase + canonical on Linux
    echo "$RELAY_ID" > "$ID_FILE"
fi
log "RELAY_ID = $RELAY_ID"

# 2. CA fingerprint — SHA-256 over the DER of the first cert served at /ca.crt
#    (matches relay/src/provision.rs::certificate_fingerprint).
if [[ -z "${RELAY_CA_FINGERPRINT:-}" ]]; then
    log "fetching CA fingerprint from http://${CONTROLLER_HTTP_ADDR}/ca.crt"
    RELAY_CA_FINGERPRINT="$(
        curl -fsS "http://${CONTROLLER_HTTP_ADDR}/ca.crt" \
            | openssl x509 -outform DER 2>/dev/null \
            | sha256sum | awk '{print $1}'
    )" || err "could not reach the controller at ${CONTROLLER_HTTP_ADDR}. Is it running?"
fi
[[ "$RELAY_CA_FINGERPRINT" =~ ^[0-9a-f]{64}$ ]] || err "derived fingerprint looks wrong: '$RELAY_CA_FINGERPRINT'"
log "RELAY_CA_FINGERPRINT = $RELAY_CA_FINGERPRINT"

# 3. Build.
log "building relay (debug)…"
cargo build --manifest-path "${REPO_ROOT}/relay/Cargo.toml"

# 4. Run in the foreground. Lower max-connections + heartbeat so capacity
#    tiers and migration/drain are easy to trigger by hand; override as needed.
log "starting relay on 0.0.0.0:${PORT}  (state: ${STATE_DIR})"
exec env \
    RELAY_ID="$RELAY_ID" \
    CONTROLLER_ADDR="$CONTROLLER_ADDR" \
    CONTROLLER_HTTP_ADDR="$CONTROLLER_HTTP_ADDR" \
    RELAY_CA_FINGERPRINT="$RELAY_CA_FINGERPRINT" \
    RELAY_BIND="0.0.0.0:${PORT}" \
    RELAY_STATE_DIR="$STATE_DIR" \
    RELAY_IP_SANS="${RELAY_IP_SANS:-127.0.0.1}" \
    RELAY_MAX_CONNECTIONS="${RELAY_MAX_CONNECTIONS:-8}" \
    RELAY_HEARTBEAT_INTERVAL_SECS="${RELAY_HEARTBEAT_INTERVAL_SECS:-10}" \
    LOG_LEVEL="${LOG_LEVEL:-debug,zecurity_relay=debug}" \
    "${REPO_ROOT}/relay/target/debug/zecurity-relay"
