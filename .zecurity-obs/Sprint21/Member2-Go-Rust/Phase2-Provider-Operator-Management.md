---
type: phase
member: M2
person: Barath
sprint: 21
phase: 2
execution: U
title: Provider Operator Management (API)
status: planned
depends_on: [1]   # needs H: session_generation, GetByID, provider key
schema_change: false   # uses the columns added by 037 in Phase H
tags:
  - go
  - provider
  - rbac
  - rest
  - audit
  - provider-dashboard
---

# Phase 2 (U) — Provider Operator Management (API)

> **Decision Record:** D-05 (roles stay `super-admin` / `relay-ops`; no role migration), D-06 (the prefix rule decides access), D-16 (`sub` binding on first login; disable revokes sessions; reconcile `PROVIDER_BOOTSTRAP_EMAILS`), D-14 (provider actions are audited in `provider_audit_logs`).
> **Why now:** Sprint 22 adds dangerous buttons (suspend tenant, relay revoke in the console). Before that, super-admins must be able to decide **who** can log in to the provider console and **with which role**. Today they can't. See "Problem".
> **The UI for this phase** is M1-C5, the "Provider users" page in [[Sprint21/Member1-Go/Phase2-Provider-Console-Foundation]].

## Problem (verified)

- **No way to manage operators.** `provider.Store.Create(email, role)` and `Store.Disable(id)` exist (`controller/internal/provider/store.go:97,112`), but **nothing calls them**. No HTTP route exists.
  - The only provider-user route is `GET /provider/users`, a super-admin-only list (`internal/provider/handler.go` `ListUsers`, `main.go:356`).
- **Only super-admins can exist.** The only way to create a provider user is `PROVIDER_BOOTSTRAP_EMAILS`, and it can only create **super-admins**. A `relay-ops` user can't be created without hand-written SQL.
- **No way to change access.** There's no way to change a role, disable an operator, or re-enable one.
- **Bootstrap reverts changes on restart.** `UpsertSuperAdmin` runs on every startup and forces each bootstrap email back to an active `super-admin` (`ON CONFLICT (email) DO UPDATE SET role='super-admin', disabled_at=NULL`, `store.go:129-150`). So disabling or demoting a bootstrap account through an API would be silently undone at the next restart.

## Goal

A super-admin can add an operator with a role, change it, disable them and re-enable them through `/provider/*`, with every change audited.
- Nobody can lock the provider plane out.
- Bootstrap accounts stay a predictable break-glass path.

## Implementation choices (reviewable; not architectural)

| Area | Choice | Why |
|------|--------|-----|
| Add operator | Pre-register the email plus role. The person signs in with Google, and Phase H binds `sub` on first login. No invitation email or token. | Reuses H's binding; nothing new to secure. |
| Bootstrap accounts | Emails listed in `PROVIDER_BOOTSTRAP_EMAILS` are **pinned by config**. The API refuses to disable or demote them (`409 managed_by_bootstrap`), and the list shows `pinned: true`. To remove one, take it out of the env var first. | Matches `UpsertSuperAdmin`'s behaviour on every startup instead of fighting it, and keeps a config-level recovery path. |
| Sessions on change | Disable, enable and role change all increment `session_generation`. | Disable must end sessions immediately (H5). A role change forces a fresh login, so the console's cached role and nav can't go stale. |
| Transactions | Each mutation and its audit row are written in **one transaction** (`InsertAuditTx`). The last-super-admin check locks the active super-admin rows (`FOR UPDATE`). | Audit can't diverge from state, and two concurrent demotions can't both succeed. |

## Routes

All routes go behind `RequireProvider` and are authorised with `CanManageProviderUser` (`provider_user.manage`, which the existing prefix rule gives to super-admin only). `relay-ops` gets 403 on all of them.

| Route | Body | Result |
|-------|------|--------|
| `GET /provider/users` (exists; extend the DTO) | — | `[{id, email, role, disabled_at, created_at, bound (google_sub IS NOT NULL), hosted_domain, pinned}]`. Never the raw `google_sub`. |
| `POST /provider/users` | `{email, role}` | **201** with the user. Email trimmed and lowercased, role validated. An active duplicate → **409 `already_exists`**. A disabled duplicate → **409 `exists_disabled`** (use enable). |
| `PATCH /provider/users/{id}` | `{role}` | **200**. Same role → 200 no-op, not audited. |
| `POST /provider/users/{id}/disable` | — | **204**; sets `disabled_at`, increments the generation |
| `POST /provider/users/{id}/enable` | — | **204**; clears `disabled_at`, increments the generation |

**Errors** are `{"error": "<code>"}`:
- 400 `invalid_email` / `invalid_role`;
- 404 `not_found`;
- 409 `already_exists` / `exists_disabled` / `managed_by_bootstrap` / `last_super_admin` / `cannot_modify_self`.

## Steps

### U1 — Store (`internal/provider/store.go`)

```go
func (s *Store) ChangeRole(ctx context.Context, actor Actor, id, role string, pinned func(email string) bool) (*ProviderUser, error)
func (s *Store) SetDisabled(ctx context.Context, actor Actor, id string, disabled bool, pinned func(email string) bool) error
func (s *Store) CreateOperator(ctx context.Context, actor Actor, email, role string) (*ProviderUser, error)
```

Each function runs **one transaction**:
1. Lock the target row (`SELECT … FOR UPDATE`).
2. Apply the guards (U2).
3. Update, including `session_generation = session_generation + 1` for role and disabled changes.
4. `InsertAuditTx`.

The `pinned` predicate comes from the parsed `PROVIDER_BOOTSTRAP_EMAILS` set. Extend `ProviderUser` with `GoogleSubBound bool`, `HostedDomain string` and `SessionGeneration int64` (the H columns).

### U2 — Guards (all enforced server-side, in the transaction)

1. **Self:** an actor can't disable or demote themselves → `cannot_modify_self`. They can still add others.
2. **Last super-admin:** an operation that would leave **zero active super-admins** is refused → `last_super_admin`.
   - "Active" means `role='super-admin' AND disabled_at IS NULL`.
   - Lock those rows `FOR UPDATE` before counting.
3. **Pinned:** disabling or demoting a `PROVIDER_BOOTSTRAP_EMAILS` account → `managed_by_bootstrap`.
4. **Role values:** only `super-admin` / `relay-ops`. The DB `CHECK` stays as a backstop.

### U3 — Handlers + routes

- Handlers go in `internal/provider/handler.go`, next to `ListUsers`, and follow its authz-chokepoint pattern.
- Wire them in `main.go` next to `GET /provider/users`, after Phase H's middleware signature change.
- Pass the bootstrap email set into the handlers. It's already parsed for seeding in `main.go`.

### U4 — Audit actions

| Action | Target | Details |
|--------|--------|---------|
| `provider_user.create` | `provider_user/<id>` | `{email, role}` |
| `provider_user.role_change` | `provider_user/<id>` | `{email, from, to}` |
| `provider_user.disable` / `provider_user.enable` | `provider_user/<id>` | `{email}` |

`provider_user.bind_sub` (first login) and `provider_session.logout` come from Phase H.

### U5 — Tests

See the table below. The role matrix gets shared review with M1, since it's part of the auth boundary.

## Invariants

1. Only super-admin can call these routes. `relay-ops` and tenant tokens can't (403 / 401).
2. There's always at least one active super-admin, even under concurrent requests.
3. Nobody can disable or demote themselves through the API.
4. Bootstrap-pinned accounts can't be disabled or demoted through the API, so a restart never contradicts an API change.
5. A disabled operator's existing tokens stop working on the next request (generation bump plus the `disabled_at` check).
6. Every successful mutation writes exactly one audit row in the same transaction. Failed or refused mutations write none.
7. No role migration and no new roles (D-05). No schema change beyond H's `037`.
8. Responses never include `google_sub`, tokens or secrets.

## Tests

| Test | Kind | Asserts |
|------|------|---------|
| `TestOperatorRoutes_RoleMatrix` | HTTP | super-admin 2xx; relay-ops 403; tenant JWT 401 |
| `TestCreateOperator_ThenFirstLoginBinds` | DB + fake IdP | a created relay-ops user logs in, gets `sub` bound, and can read relays but not tenants |
| `TestCreateOperator_Duplicates409` | DB | active → `already_exists`; disabled → `exists_disabled`; email case-insensitive |
| `TestChangeRole_BumpsGenerationAndAudits` | DB | old token 401; one `provider_user.role_change` row with from/to |
| `TestDisable_EndsSessions_Enable_Restores` | DB | token 401 after disable; after enable, a new login works and the old token stays invalid |
| `TestGuard_CannotModifySelf` | DB | self-disable and self-demote → 409 |
| `TestGuard_LastSuperAdmin` | DB | demoting or disabling the only active super-admin → 409 |
| `TestGuard_LastSuperAdmin_Concurrent` | DB | two super-admins demote each other concurrently → exactly one succeeds |
| `TestGuard_PinnedBootstrap` | DB | disable or demote a bootstrap email → `managed_by_bootstrap`; restart seeding leaves state consistent |
| `TestOperatorMutations_AuditOnlyOnSuccess` | DB | refused and failed calls write no audit row |
| `TestListUsers_NoRawSub` | HTTP | `bound` present; `google_sub` absent |

DB-backed tests use the throwaway-DB harness. **A skipped DB test fails acceptance.**

## Acceptance Criteria

- [ ] AT-U.1 … AT-U.8 in [[Sprint21/Acceptance-Test-Plan]] pass.

## Build Check

```bash
cd controller && go build ./... && go vet ./... && go test ./internal/provider/... ./internal/middleware/...
```

## Implementation Checklist

- [ ] U1 store functions (transactional, generation bump, audit)
- [ ] U2 guards: self, last super-admin (locked), pinned bootstrap, role values
- [ ] U3 handlers + routes; bootstrap set passed in
- [ ] U4 audit actions
- [ ] U5 tests; build gate
- [ ] Tell M1 the API is merged (it unblocks M1-C5)

## Post-Phase Fixes

_None yet._
