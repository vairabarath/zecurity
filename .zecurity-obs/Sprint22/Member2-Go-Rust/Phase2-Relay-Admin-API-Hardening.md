---
type: phase
member: M2
person: Barath
sprint: 22
phase: 2
execution: A
title: Relay Admin API Hardening (D-17, D-31)
status: done   # 2026-10-26: implemented (da0a1613), live AT-A PASS on an isolated local stack; FU-1 open as a separate follow-up
depends_on: [1]   # T merged (D-33)
schema_change: false
tags:
  - go
  - provider
  - relay
  - rbac
  - audit
  - provider-dashboard
---

# Phase 2 (A) — Relay Admin API Hardening

> **Decision Record:** **D-31** (revoke/delete super-admin only, server-side; amends D-05), **D-17** (mandatory reason + typed confirmation), **D-14** (audit), **D-20** (replace, don't re-issue).
>
> **Why:** the relay admin routes were built for `curl`. Before the console calls them they need console-grade errors, the D-17 safeguards and the D-31 role split.

## Problem (verified 2026-10-26, `controller/internal/relay/admin_handler.go`)

- Errors are plain text (`http.Error`), and `Create`, `Revoke` and `Delete` append `err.Error()` to 500 responses.
- `Revoke` takes its reason from an **optional** `?reason=` query parameter; `Delete` stores a fixed `"relay deleted"`. Neither checks a typed confirmation (D-17).
- Under `decide()`'s `relay.*` prefix rule, **relay-ops can revoke and delete** (D-31 says super-admin only).
- The audit IP is `r.RemoteAddr` (includes the port); provider auth uses `clientIP(r)` (unexported).
- Revoking an already revoked relay, or acting on a deleted one, is a silent 204 (`RevokeRelay` is idempotent).
- The `Delete` doc comment still says it "does NOT revoke the certificate", but it does (revoke-then-remove).

## Goal

The three relay admin routes return JSON error codes only, enforce D-31 in `decide()`, require a reason and the typed relay name for revoke/delete (checked by the server), answer repeat actions with clear 409s, and audit the operator's reason and client IP.

## Contract

| Route | Allowed | Body | Success | Errors |
|-------|---------|------|---------|--------|
| `POST /provider/relays` | super-admin, relay-ops | `{name, dns_allowlist, ip_allowlist}` | **201** (fields extended in Phase E) | 400 `invalid_request` / `invalid_name` / `invalid_dns_san` / `invalid_ip_san` |
| `POST /provider/relays/{id}/revoke` | **super-admin** | `{reason, confirm_name}` | **204** | 400 `reason_required` / `reason_too_long` / `confirmation_mismatch`; 403 `forbidden`; 404 `not_found`; 409 `already_revoked` / `already_deleted` |
| `DELETE /provider/relays/{id}` | **super-admin** | `{reason, confirm_name}` | **204** (revoked relay may be deleted) | as revoke, minus `already_revoked` |

- `reason`: trimmed, 1–500 characters. Stored in the audit `details.reason` and in `relay_certificates.revocation_reason`.
- `confirm_name`: must equal `relays.name` exactly (checked inside the transaction, after the row lock).
- Audit IP: `provider.ClientIP(r)`.
- 500 → `{"error":"server_error"}`; the cause goes to the log only.

## Changes

1. `authz.go` `decide()`: for `relay-ops`, allow the `relay.` prefix **except** `relay.revoke` and `relay.delete`. Comment the exception with D-31.
2. Export thin wrappers `provider.WriteError` and `provider.ClientIP` over the existing `writeAuthError` / `clientIP`.
3. `admin_handler.go`: JSON errors; body parsing for revoke/delete; reason/confirmation validation; repeat-action 409s (status read under `FOR UPDATE` in `RevokeRelay`'s transaction, via a small store change that returns the prior status and name); doc comment fixed.

## Tests (write first)

- **Role matrix:** relay-ops → 403 on revoke and delete, 2xx on create; super-admin → 2xx on all three; unknown role → 403.
- **Validation:** missing/blank/over-long reason; missing or wrong `confirm_name` (case-sensitive); malformed JSON.
- **Repeat actions:** revoke twice → second 409 `already_revoked`; revoke after delete → 409 `already_deleted`; delete after revoke → 204.
- **Audit:** one row per success with actor, `reason`, `serials_revoked`, client IP without port; **no** row on any 4xx; audit failure rolls back the status and revocation.
- **Fan-out:** `OnRelayRevoked` called exactly once per success, never on failure.
- **No leakage:** 500 bodies are exactly `{"error":"server_error"}`.

## Acceptance

[[Sprint22/Acceptance-Test-Plan]] → **AT-A1 … AT-A6**.

## Test gate

Go gate from [[Sprint22/path]] (`internal/provider`, `internal/relay`), DB-backed.

## Implementation record (2026-10-26)

**Files:**
- `controller/internal/provider/authz.go`: `superAdminOnlyRelayActions` (D-31 exception in `decide()`).
- `controller/internal/provider/httpx.go`: `WriteJSON`, `WriteError`, `WriteErrorDetail`, `ClientIP`, `DecodeJSON`, thin wrappers over the existing auth helpers.
- `controller/internal/relay/store.go`: `RevokeRelayGuarded` (name + status guard under `FOR UPDATE`); `RevokeRelay` delegates with a nil guard, so existing callers and tests are unchanged.
- `controller/internal/relay/admin_handler.go`: JSON error codes; UUID check before any query; D-17 body; guard; client IP; Create bounded decoding, name rules, SAN error codes, `201` + `Cache-Control: no-store`.
- `docs/sprint20-live-acceptance-runbook.md`: the revoke command now sends the JSON body (`R1_NAME` added).

**Decisions and notes:**
- Create now answers **201 Created** (was 200), as documented in this phase file.
- `ClientIP` uses the connection address only; `X-Forwarded-For`, `X-Real-IP` and `Forwarded` are ignored (no trusted-proxy boundary). A test pins this.
- **Known gap FU-1 (D-14):** Create's audit stays best-effort; see [[Sprint22/path]] → Follow-ups. Revoke and delete roll the whole mutation back when the audit write fails (tested).

**Tests (written first, confirmed failing first):** `TestDecide_RelayMutationMatrix`; `internal/relay/admin_handler_db_test.go`, DB-backed:
- the role matrix over HTTP;
- validation codes, including the exact, case-sensitive name match;
- repeat revoke/revoke, revoke/delete and delete/revoke;
- concurrent revoke/revoke, revoke/delete and delete/delete;
- audit failure rolls back and leaks nothing;
- forwarding headers are ignored;
- invalid Create leaves no relay or audit row;
- relay-ops Create gets `no-store`, with no token in the audit.

**Live acceptance (2026-10-26): PASS.** 38 checks, 0 failures, on an isolated local stack with a real provisioned relay; see [[Sprint22/Acceptance-Test-Plan]] → "Live run — Phase A". The one remaining observation is connector migration after a revoke, which needs a connector and is covered by AT-X4.

## Post-Phase Fixes

*(None yet.)*
