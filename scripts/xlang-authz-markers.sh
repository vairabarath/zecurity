#!/usr/bin/env bash
# Cross-language authorization-contract test (connector-revocation Phase A).
#
# Builds the Go harness that serves the PRODUCTION controller Control handler
# over grpc-go, then runs the Rust integration test that drives it with the
# PRODUCTION tonic client and marker classifier
# (connector/tests/xlang_authz_markers.rs).
#
# Needs: go, cargo, a reachable Postgres admin DSN (the harness creates and
# drops its own throwaway database).
#
#   XLANG_DB_URL=postgres://ztna:…@localhost:5432/ztna_platform?sslmode=disable \
#     scripts/xlang-authz-markers.sh
#
# If XLANG_DB_URL is unset, DATABASE_URL from controller/.env is used.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if [[ -z "${XLANG_DB_URL:-}" && -f "$root/controller/.env" ]]; then
  XLANG_DB_URL="$(grep -E '^DATABASE_URL=' "$root/controller/.env" | head -1 | cut -d= -f2-)"
fi
: "${XLANG_DB_URL:?set XLANG_DB_URL (Postgres admin DSN)}"

bin_dir="$(mktemp -d)"
trap 'rm -rf "$bin_dir"' EXIT

(cd "$root/controller" && go build -tags xlangharness -o "$bin_dir/xlangharness" ./internal/connector/xlangharness)

export ZECURITY_XLANG_HARNESS="$bin_dir/xlangharness"
export XLANG_DB_URL
export XLANG_MIGRATIONS_DIR="$root/controller/migrations"

cd "$root/connector"
cargo test --test xlang_authz_markers -- --ignored --nocapture
