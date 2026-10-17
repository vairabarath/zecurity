#!/usr/bin/env bash
# Export the platform root CA certificate from the controller database, for
# embedding in zecurity-client (certificate audit bug 1).
#
# Reads only ca_root.certificate_pem: the public certificate, never the
# encrypted private key. Read-only.
#
# Usage:
#   scripts/export-platform-root.sh [OUT_FILE]      (default: stdout)
#
# Database:
#   DATABASE_URL=postgres://...   use psql against this URL, or
#   (unset)                       docker exec into the local compose Postgres
#                                 (POSTGRES_CONTAINER, default ztna_postgres;
#                                 POSTGRES_USER ztna; POSTGRES_DB ztna_platform)
set -euo pipefail

out="${1:--}"
query="SELECT certificate_pem FROM ca_root"

if [[ -n "${DATABASE_URL:-}" ]]; then
  pem="$(psql "$DATABASE_URL" -At -c "$query")"
else
  pem="$(docker exec "${POSTGRES_CONTAINER:-ztna_postgres}" \
    psql -U "${POSTGRES_USER:-ztna}" -d "${POSTGRES_DB:-ztna_platform}" -At -c "$query")"
fi

count="$(grep -c -- '-----BEGIN CERTIFICATE-----' <<<"$pem" || true)"
if [[ "$count" != 1 ]]; then
  echo "export-platform-root: expected exactly one root certificate in ca_root, found $count" >&2
  echo "  (has the controller started once, so its PKI is initialised?)" >&2
  exit 1
fi

if [[ "$out" == "-" ]]; then
  printf '%s\n' "$pem"
else
  printf '%s\n' "$pem" >"$out"
  echo "export-platform-root: wrote $out" >&2
fi

if command -v openssl >/dev/null 2>&1; then
  printf '%s\n' "$pem" | openssl x509 -noout -subject -fingerprint -sha256 >&2
fi
