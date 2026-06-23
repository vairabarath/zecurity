#!/usr/bin/env bash
# Sprint 10.2 final build/test gate — matches Phase 3 phase doc.
# Exits non-zero on first failure so CI / manual runs surface the breakage early.

set -euo pipefail
cd "$(dirname "$0")/.."

step() { echo; echo "==> $*"; }

step "buf generate"
buf generate

step "controller — go test + build"
( cd controller && go test ./internal/policy/... ./internal/client/... ./internal/connector/... \
  && go build ./... )

step "relay — cargo test + build"
( cd relay && cargo test && cargo build )

step "connector — cargo test + build"
( cd connector && cargo test && cargo build )

step "client — cargo test + build"
( cd client && cargo test && cargo build )

echo
echo "==> All Sprint 10.2 gates passed."
