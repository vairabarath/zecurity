---
type: planning
status: complete
sprint: 12
tags:
  - sprint12
  - dependencies
  - execution-path
  - team-coordination
  - provider-plane
  - provider-identity
  - relay-provisioning
  - security
  - pki
---

# Sprint 12 — Provider Identity Tier + Authenticated Relay Provisioning

> **Read this before writing a single line of code.**
> Source of truth for scope: `.zecurity-obs/Decisions/ADR-021-Provider-Identity-and-Authorization.md`
> and `.zecurity-obs/Decisions/ADR-020-Authenticated-Relay-Provisioning.md`
> (accepted 2026-07-13; formerly PENDING-07a / PENDING-01).
> Detailed engineering plan: `~/Documents/PLAN-PENDING-01-and-07a-Provider-Tier-and-Relay-Provisioning.md`.

## Sprint Goal

Close the **anonymous CA-signing hole** in relay provisioning — *correctly* — by first standing up
the **provider identity/authorization boundary** that relay creation and token issuance must sit
behind.

This sprint completes two Phase-0 items from the pending roadmap, shipped **together** because they
are two halves of one lock:

- **PENDING-07a (M1)** — makes relay-token issuance a **provider** action: a separate
  `provider_users` tier, `RequireProvider` middleware, a `/provider` API surface, a single authz
  chokepoint, and per-action audit.
- **PENDING-01 (M2)** — makes the issued token **actually enforced**: the `Provision` RPC verifies
  and burns the single-use token before signing, and the anonymous self-provision fallback is removed.

```text
BEFORE                                          AFTER
------                                          -----
POST /api/relays  (RequireRole("admin"),        POST /provider/relays  (RequireProvider,
  no WorkspaceGuard → any tenant admin)           provider allowlist + authz + audit)

Provision(token) → token ignored,               Provision(token) → verify + burn single-use JTI,
  unknown UUID self-inserts active relay           unregistered relay rejected (no self-insert)
```

A token issued but never verified is decorative; a `Provision` that verifies tokens mintable by any
tenant admin is still a privilege gap. Neither half alone is sufficient.

## Key Design Decisions

| Decision | Detail |
|----------|--------|
| Separate identity tier (07a Option A) | New `provider_users` store — **not** the tenant `users` table. |
| Separate provider audit | New `provider_audit_logs`; the tenant `audit_logs.tenant_id` is `NOT NULL` and cannot hold provider actions. Append-only, with `details JSONB`. |
| Audience-scoped session | Reuse the existing Google OIDC exchange but issue a provider JWT with `aud=provider` + a distinct cookie, so a tenant JWT can never pass `RequireProvider`. |
| No new IdP for alpha | Consumer Google OIDC; the `provider_users` allowlist + network isolation are the boundary. Enterprise SSO/MFA deferred. |
| Single authz chokepoint | One `Authz.decide(actor, action, target)`; typed wrappers (`CanCreateRelay`, `CanIssueProvisioningToken`, …). Signatures **carry `actor`+`target` now** so partner-scoping is a later policy change, not a signature change. |
| Simple role enum | `super-admin` (all) / `relay-ops` (relay.*). No per-user permission rows yet. |
| Flat data model | No `provider_org_id` column yet — added when partners are real. |
| Bootstrap via env | Super-admin #0 seeded from `PROVIDER_BOOTSTRAP_EMAILS` (idempotent). No self-registration. |
| CLI/API-driven alpha | No React provider console yet — that is PENDING-07b (Beta). The `/provider` API is what it will consume. |
| Reuse existing token machinery | `token.go` `VerifyProvisioningToken` / `BurnProvisioningJTI` already exist with **zero callers** — wire them, don't rewrite. |

## Team Assignments

| Member | Role | Area |
|--------|------|------|
| **M1** | Go (Provider tier) | Migrations 025/026, `internal/provider` (store + authz chokepoint), provider session, `RequireProvider`, `/provider` route group, bootstrap seed |
| **M2** | Go + Rust (Relay provisioning) | `Provision` token verify/burn, drop self-provision fallback, re-home `POST /provider/relays`, Rust relay client token delivery |

## Critical Rule: Conflict Zones

| File | Who Touches It | Rule |
|------|----------------|------|
| `controller/cmd/server/main.go` | M1 + M2 | **M1 lands provider wiring + `/provider` route group + bootstrap first; M2 rebases** and adds `WithProvisioningAuth` + the relay route move. |
| `controller/migrations/025_provider_users.sql` | M1 | M1 owns. Latest existing migration is `024`; do not reuse numbers. |
| `controller/migrations/026_provider_audit_logs.sql` | M1 | M1 owns. |
| `controller/internal/provider/` | M1 | M1 owns store + authz chokepoint. |
| `controller/internal/middleware/provider.go` | M1 | M1 owns `RequireProvider`. |
| `controller/internal/auth/` | M1 | M1 adds provider-audience session issuance; coordinate before touching shared session code. |
| `controller/internal/relay/provision.go` | M2 | M2 owns token enforcement + fallback removal. |
| `controller/internal/relay/admin_handler.go` | M2 | M2 (re-home) touches; **consumes M1's `Authz`** — do not duplicate authz logic. |
| `relay/src/provision.rs` | M2 | M2 owns client token delivery. |

## Dependency Graph

```text
M1 Phase 1 — Provider data model + authz core        (independent, Day 1)
  ↓
M1 Phase 2 — Provider session + RequireProvider + /provider route group
  ↓
M2 Phase 2 — Re-home POST /provider/relays           (needs M1 P1 + P2)

M2 Phase 1 — Provision token verify/burn + drop fallback   (independent, Day 1)
  ↓
M2 Phase 3 — Relay client sends token                (needs M2 P1 server-side)
```

> Day-1 parallelism: **M1 Phase 1** and **M2 Phase 1** have no dependencies and start immediately.

## Execution Path

### Phase A — M1: Provider Data Model + Authz Core

> See [[Sprint12/Member1-Go/Phase1-Provider-Data-Model-Authz]].
> Depends on nothing — Day 1.

- [x] **M1-A1** `controller/migrations/025_provider_users.sql` — `provider_users` (email UNIQUE, role, disabled_at).
- [x] **M1-A2** `controller/migrations/026_provider_audit_logs.sql` — append-only provider audit with `details JSONB`.
- [x] **M1-A3** `controller/internal/provider/store.go` — `ProviderStore`: GetByEmail / List / Create / Disable / InsertAudit.
- [x] **M1-A4** `controller/internal/provider/authz.go` — `Authz` chokepoint; typed `CanX(actor, target)` → `decide(actor, action, target)`.
- [x] **M1-A5** `controller/cmd/server/main.go` — bootstrap seed from `PROVIDER_BOOTSTRAP_EMAILS` (idempotent upsert as `super-admin`).
- [x] **Build gate:** `cd controller && go build ./...`

### Phase B — M1: Provider Session + RequireProvider + /provider route group

> See [[Sprint12/Member1-Go/Phase2-Provider-Session-Middleware]].
> Depends on Phase A.

- [x] **M1-B1** `controller/internal/auth/*` — issue provider JWT with `aud=provider` (reuse Google exchange); `/provider/auth/callback` gated by allowlist.
- [x] **M1-B2** `controller/internal/middleware/provider.go` — `RequireProvider`: enforce `aud=provider`, allowlist + `disabled_at` check, inject provider ctx, **never** call `WorkspaceGuard`.
- [x] **M1-B3** `controller/cmd/server/main.go` — `/provider` route group skeleton behind `AuthMiddleware(provider) → RequireProvider`.
- [x] **Build gate:** `cd controller && go build ./...`

### Phase C — M2: Provision Token Enforcement

> See [[Sprint12/Member2-Go-Relay/Phase1-Provision-Token-Enforcement]].
> Depends on nothing — Day 1.

- [x] **M2-C1** `controller/internal/relay/provision.go` — add JWT-secret field + `WithProvisioningAuth`.
- [x] **M2-C2** `provision.go` — verify token → assert `claims.RelayID == relayID` → `BurnProvisioningJTI` **before** `SignRelayCert`.
- [x] **M2-C3** `provision.go` — drop self-provision fallback (lines ~123-138); return `FailedPrecondition` on `ErrRelayNotFound`.
- [x] **M2-C4** `controller/cmd/server/main.go` — wire `WithProvisioningAuth(JWT_SECRET)` at relay-service construction.
- [x] **M2-C5** `provision_test.go` — valid / missing / wrong-relay / replay / unregistered cases.
- [x] **Build gate:** `cd controller && go build ./...`

### Phase D — M2: Re-home Relay Management under /provider

> See [[Sprint12/Member2-Go-Relay/Phase2-Rehome-Relay-Under-Provider]].
> Depends on Phase B (M1 `RequireProvider` + `Authz`).

- [x] **M2-D1** `controller/cmd/server/main.go` — remove `POST /api/relays`; add `POST /provider/relays` behind `RequireProvider`.
- [x] **M2-D2** `controller/internal/relay/admin_handler.go` — call `Authz.CanIssueProvisioningToken(actor, target)`; write `provider_audit_logs` (`relay.create`).
- [x] **M2-D3** (optional) stub `DELETE /provider/relays/{id}` guarded by `CanDeleteRelay` + audit — seam for PENDING-02 (not wired to CRL).
- [x] **Build gate:** `cd controller && go build ./...`

### Phase E — M2: Relay Client Token Delivery

> See [[Sprint12/Member2-Go-Relay/Phase3-Relay-Client-Token]].
> Depends on Phase C (server accepts the token).

- [x] **M2-E1** `relay/src/provision.rs` — send the real `provisioning_token` (not `String::new()`).
- [x] **M2-E2** relay config — `RELAY_PROVISIONING_TOKEN` env / file source; document delivery in the relay README.
- [x] **M2-E3** fail fast with a clear operator error when the token is missing.
- [x] **Build gate:** `cd relay && cargo build`

## Final Build Gates

```bash
cd controller && go build ./...
cd controller && go test ./internal/relay/... ./internal/provider/...
cd relay && cargo build
```

## Acceptance Criteria

- [x] `Provision` requires a valid single-use token; the JTI is burned atomically before signing.
- [x] A replayed token is rejected (`PermissionDenied`).
- [x] A token whose `relay_id` ≠ request `relay_id` is rejected.
- [x] The self-provision fallback is gone; an unregistered relay is rejected (`FailedPrecondition`).
- [x] `POST /api/relays` no longer exists; relay creation is `POST /provider/relays` behind `RequireProvider`.
- [x] A tenant JWT is rejected by `RequireProvider`; a provider JWT is rejected by tenant `AuthMiddleware`.
- [x] Every provider action writes a `provider_audit_logs` row (actor + target + details).
- [x] Super-admin #0 is seeded from `PROVIDER_BOOTSTRAP_EMAILS` on startup.
- [ ] The relay client sends its provisioning token and provisions end-to-end.

## Deferred

- Full provider RBAC granularity + separation-of-duties enforcement.
- Partner/reseller `provider_org_id` scoping (signatures already carry `target`).
- Break-glass tenant impersonation.
- Enterprise SSO / multiple IdPs / first-party MFA (PENDING-04/06).
- React provider console (PENDING-07b) — Beta.
- CRL/OCSP revocation enforcement (PENDING-02) — the relay-revoke trigger seam is left under `/provider`.

## Notes for AI Agents Working on This Sprint

1. Read this `path.md` fully, then your first unchecked phase whose `depends_on` are all satisfied.
2. Cross-check every phase against `~/Documents/PLAN-PENDING-01-and-07a-Provider-Tier-and-Relay-Provisioning.md`.
3. **Never renumber proto fields.** No proto changes are expected this sprint.
4. `main.go` is a shared conflict zone — respect the M1-first rule above.
5. On completion, promote `PENDING-01` and `PENDING-07a` to the next free `ADR-0NN` in `.zecurity-obs/Decisions/`, record fixes in each phase file's Post-Phase Fixes, and append a Session Log entry.
