---
type: phase
member: M1
sprint: 17
phase: 2
title: SCIM Token Authentication
status: done
depends_on: [1]
tags: [go, identity, scim, auth, tokens, pending-05]
---

# Phase 2 — SCIM Token Authentication

> Depends on Phase 1. Full spec: [[ADR-025-SCIM-Directory-Synchronization]] §7 · [[PENDING-05-SCIM-Implementation-Plan]] P2.

## Goal
Per-`(workspace, connection)` bearer tokens for the SCIM endpoints — machine-to-machine, never a user JWT.

## Files
| File | Change |
| --- | --- |
| `controller/internal/scim/token_store.go` | **new** — mint/rotate/revoke/lookup |
| `controller/internal/scim/middleware.go` | **new** — bearer auth for `/scim/v2/*` |
| `controller/graph/idp.graphqls` + resolvers | SCIM token admin mutations/queries |

## Steps
- [x] `token_hash` = **HMAC-SHA256** of a 256-bit random token, keyed by new env `SCIM_TOKEN_HASH_KEY` (separate from `PKI_MASTER_SECRET`); store only the digest.
- [x] Dual-token rotation: ≤2 active per scope, 24h grace (`SCIM_TOKEN_ROTATION_GRACE_HOURS`), never extend an earlier expiry, row-lock on rotate; explicit revoke; `scim.token.auto_expire` event.
- [x] Bearer middleware: lookup-by-hash → bind `(workspace_id, connection_id)`; `last_used_at`; `401` fail-closed on bad/expired/revoked.
- [x] GraphQL mint (plaintext shown once) / rotate / revoke / list; all audited.

## Rules
- A token for `(A,X)` must never touch `(B,·)` or `(A,Y)`. Scope is the pair, never workspace-only.

## Build gate
`go build ./...` + unit tests: rotation/grace math, scope isolation, HMAC lookup.
