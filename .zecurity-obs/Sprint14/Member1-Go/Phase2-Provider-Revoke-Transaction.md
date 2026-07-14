---
type: phase
member: M1
sprint: 14
phase: 2
title: Provider Revoke Transaction
status: planned
depends_on:
  - Sprint14/Member1-Go/Phase1-Relay-Cert-History-Data-Model
tags: [go, relay, provider, authz, audit, revocation, pending-02]
---

# Phase 2 — Provider Revoke Transaction

> Depends on Phase 1 (`RevokeAllForRelay`). Reuses the Sprint-12 provider authz + audit chokepoint.

## Goal

Make relay revocation a single **atomic** provider action: revoke every unexpired serial → drop the
relay from the advertised pool → audit → broadcast. Add an explicit revoke endpoint and redefine
`DELETE` as revoke-then-remove, **never** a physical delete.

## Files

| File | Change |
|------|--------|
| `controller/internal/provider/authz.go` | add `ActionRelayRevoke` + `CanRevokeRelay` |
| `controller/internal/relay/admin_handler.go` | add `Revoke` handler; change `Delete` semantics |
| `controller/cmd/server/main.go` | route `POST /provider/relays/{id}/revoke`; ensure broadcaster wired |

## authz.go
- Add `ActionRelayRevoke = "relay.revoke"`.
- Add `CanRevokeRelay(actor, target) error` → `decide(actor, ActionRelayRevoke, target)`. `relay-ops`
  already covers the `relay.*` namespace, so both roles pass — no `decide` change needed.

## admin_handler.go
`Revoke(w, r)` (behind `requireProvider`, mirrors the Sprint-12 `Create`/`Delete` pattern):
1. `actor, ok := provider.ActorFromContext` → 500 if missing.
2. `relayID := r.PathValue("id")` → 400 if empty.
3. `h.Authz.CanRevokeRelay(actor, Target{Type:"relay", ID: relayID})` → 403 on `ErrForbidden`.
4. **Atomic flow:**
   - `n, err := h.Store.RevokeAllForRelay(ctx, relayID, reason)` (reason from body/query, optional).
   - mark the relay non-active (reuse `MarkDeleted` / a status flip) — **row preserved**.
   - `h.ProviderStore.InsertAudit(ctx, AuditEntry{Action: provider.ActionRelayRevoke, TargetType:"relay", TargetID: relayID, Details: {"serials_revoked": n, "reason": reason}})` (best-effort log on failure).
   - trigger `broadcastRelayList` (see main.go wiring) so connectors migrate immediately.
5. `204 No Content` (or `404` if the relay/serials are unknown).

`Delete(w, r)` — change from "soft-delete only" to **revoke-then-remove**: call `RevokeAllForRelay`
first (so the cert is on the CRL), then mark removed. **Do not** `DELETE FROM relays` or
`relay_certificates` — the CRL needs those rows until `not_after`. Audit action stays `relay.delete`.

## main.go
- `mux.Handle("POST /provider/relays/{id}/revoke", requireProvider(http.HandlerFunc(relayAdminHandler.Revoke)))`.
- Give `relayAdminHandler` access to the broadcaster (`broadcastRelayList`, `main.go:151-159`) so the
  revoke path can fan out a fresh `LabelledRelayList`.

## Tests
- `Revoke`: forbidden role → 403; no actor → 500; success → 204 + a `relay.revoke` audit row + broadcast fired + relay row still present.
- `Delete`: revokes serials before removal; relay + `relay_certificates` rows remain in the DB.

## Build Check
```bash
cd controller && go build ./...
```

## Implementation Checklist
- [ ] **M1-B1** `ActionRelayRevoke` + `CanRevokeRelay`.
- [ ] **M1-B2** `Revoke` handler — atomic revoke + non-active + audit + broadcast.
- [ ] **M1-B3** `Delete` = revoke-then-remove; row preserved; never hard-delete.
- [ ] **M1-B4** `POST /provider/relays/{id}/revoke` route + broadcaster wiring.
- [ ] **Build gate:** `cd controller && go build ./...`

## Pre-Implementation Corrections (validated review — codex)
- **True atomicity (must-fix).** `RevokeAllForRelay` + status update + `InsertAudit` as separate pool
  calls are **not** atomic, and "best-effort audit" contradicts an atomic revoke. Wrap **all three in
  one DB transaction** (revocation rows + relay status + audit insert), `Commit`, and only **then**
  refresh the in-memory checker and broadcast.
- **Cache refresh timing (must-fix).** Do **not** refresh the checker "inside the transaction" — a
  refresh on another pooled connection can't see uncommitted rows, and refreshing pre-commit can
  cache an uncommitted revocation. Publish the revoked serials to the cache **after commit**, then
  broadcast.
- **Revoke races Provision (security must-fix).** `provision.go:141-171` burns the token and signs
  **before** `MarkProvisioned`, and `MarkProvisioned` (`store.go:103`) sets `status='active'`
  **unconditionally (no status guard)**. A concurrent/subsequent Provision can therefore **undo a
  revoke** and issue a fresh unrevoked cert. Fix: add a **status guard** to `MarkProvisioned`
  (`WHERE id=$1 AND status='pending'`, or reject when the relay is revoked), make Provision's
  token-burn→sign→history-insert→activate conditional/transactional, and ensure revoke **serializes
  against** provisioning. (Touches `store.go` + `relay/provision.go` — coordinate with Phase 1/4.)

## Post-Phase Fixes
_None yet._
