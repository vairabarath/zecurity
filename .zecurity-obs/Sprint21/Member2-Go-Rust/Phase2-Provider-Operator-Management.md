---
type: phase
member: M2
person: Barath
sprint: 21
phase: 2
execution: U
title: Provider Operator Management (API)
status: planned
depends_on: [1]   # needs H: local accounts, password hashing, session_generation, GetByID, provider key
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

> **Decision Record:**
> - **D-27** (operator lifecycle: add, change role, disable, re-enable, **reset password**; no self-disable; never remove the last super-admin; audited);
> - **D-05** (roles stay `super-admin` / `relay-ops`);
> - **D-25** (`session_generation` revocation);
> - **D-14** (audit in `provider_audit_logs`).
>
> **Why now:** Sprint 22 adds dangerous buttons (suspend tenant, relay revoke in the console). Before that, super-admins must be able to decide **who** can sign in to the provider console and **with which role**, without SQL.
> **The UI for this phase** is M1-C5, the "Provider users" page in [[Sprint21/Member1-Go/Phase2-Provider-Console-Foundation]].

## Problem (verified)

- **No way to manage operators.** `provider.Store.Create(email, role)` and `Store.Disable(id)` exist (`controller/internal/provider/store.go:97,112`), but **nothing calls them**. No HTTP route exists.
  - The only provider-user route is `GET /provider/users`, a super-admin-only list (`internal/provider/handler.go` `ListUsers`, `main.go:356`).
- **After Phase H, only the bootstrap account exists.** There's no way to create a `relay-ops` operator, change a role, disable or re-enable someone, or reset a forgotten password, short of SQL.

## Goal

A super-admin can add an operator with a role, change it, disable and re-enable them, and reset their password through `/provider/*`, with every change audited and nobody able to lock the provider plane out.

## Implementation choices (reviewable; not architectural)

| Area | Choice | Why |
|------|--------|-----|
| Initial and reset passwords | The **server generates** a random temporary password (20 characters, `crypto/rand`), stores only its Argon2id hash with `must_change_password=true`, and returns it **once** in the response. The super-admin passes it to the operator out of band. The operator must change it at first login (the Phase H `pwc` flow). | Super-admins never choose other people's passwords, and the plaintext exists only in one response. There's no email infrastructure to depend on. |
| Sessions on change | Role change, disable, enable and password reset all bump `session_generation`. | Disable and reset must end sessions immediately. A role change forces a fresh login, so the console's cached role and nav can't go stale. |
| Transactions | Each mutation and its audit row are written in **one transaction** (`InsertAuditTx`). The last-super-admin check locks the active super-admin rows (`FOR UPDATE`). | Audit can't diverge from state, and two concurrent demotions can't both succeed. |
| Bootstrap account | No special treatment. Phase H's bootstrap is create-only, so a restart never undoes an API change. | The earlier "pinned by config" rule existed only because Google-era bootstrap re-activated accounts on every start. |

## Routes

All routes go behind `RequireProvider` and are authorised with `CanManageProviderUser` (`provider_user.manage`, super-admin only by the existing prefix rule). `relay-ops` gets 403, and a `pwc` token gets 403 `password_change_required` (Phase H).

| Route | Body | Result |
|-------|------|--------|
| `GET /provider/users` (exists; extend the DTO) | — | `[{id, email, role, disabled_at, created_at, last_login_at, must_change_password, has_password}]`. **Never** `password_hash`. |
| `POST /provider/users` | `{email, role}` | **201** `{user, temporary_password}`, with the password shown once. Email trimmed and lowercased, role validated. An active duplicate → **409 `already_exists`**. A disabled duplicate → **409 `exists_disabled`** (use enable). |
| `PATCH /provider/users/{id}` | `{role}` | **200**. Same role → 200 no-op, not audited. |
| `POST /provider/users/{id}/disable` | — | **204**; sets `disabled_at`, bumps the generation |
| `POST /provider/users/{id}/enable` | — | **204**; clears `disabled_at`, bumps the generation |
| `POST /provider/users/{id}/reset-password` | — | **200** `{temporary_password}`, shown once. New hash, `must_change_password=true`, generation bumped. |

**Errors** are `{"error": "<code>"}`:
- 400 `invalid_email` / `invalid_role`;
- 404 `not_found`;
- 409 `already_exists` / `exists_disabled` / `last_super_admin` / `cannot_modify_self`.

Responses carrying `temporary_password` set `Cache-Control: no-store`.

## Steps

### U1 — Store (`internal/provider/store.go`)

```go
func (s *Store) CreateOperator(ctx context.Context, actor Actor, email, role, passwordHash string) (*ProviderUser, error)
func (s *Store) ChangeRole(ctx context.Context, actor Actor, id, role string) (*ProviderUser, error)
func (s *Store) SetDisabled(ctx context.Context, actor Actor, id string, disabled bool) error
func (s *Store) ResetPassword(ctx context.Context, actor Actor, id, passwordHash string) error
```

Each function runs **one transaction**:
1. Lock the target row (`SELECT … FOR UPDATE`).
2. Apply the guards (U2).
3. Update, including `session_generation = session_generation + 1` where the table above says so.
4. `InsertAuditTx`.

The handler generates the temporary password and hashes it with Phase H's `password.go` **before** calling the store. The plaintext never reaches the store or the audit.

### U2 — Guards (all enforced server-side, in the transaction)

1. **Self:** an actor can't disable, demote or reset-password themselves → `cannot_modify_self`. Their own password changes go through `POST /provider/auth/password`.
2. **Last super-admin:** an operation that would leave **zero active super-admins** is refused → `last_super_admin`.
   - "Active" means `role='super-admin' AND disabled_at IS NULL`.
   - Lock those rows `FOR UPDATE` before counting.
3. **Role values:** only `super-admin` / `relay-ops`. The DB `CHECK` stays as a backstop.

### U3 — Handlers + routes

- Handlers go in `internal/provider/handler.go`, next to `ListUsers`, and follow its authz-chokepoint pattern.
- Wire them in `main.go` next to `GET /provider/users`, after Phase H's middleware change.

### U4 — Audit actions

| Action | Target | Details |
|--------|--------|---------|
| `provider_user.create` | `provider_user/<id>` | `{email, role}` |
| `provider_user.role_change` | `provider_user/<id>` | `{email, from, to}` |
| `provider_user.disable` / `provider_user.enable` | `provider_user/<id>` | `{email}` |
| `provider_user.password_reset` | `provider_user/<id>` | `{email}`. **Never the password.** |

`provider_session.login`, `provider_session.logout`, `provider_user.password_change`, `provider_user.bootstrap_create` and `provider_user.bootstrap_reset` come from Phase H.

### U5 — Tests

See the table below. The role matrix gets shared review with M1, since it's part of the auth boundary.

## Invariants

1. Only super-admin can call these routes. `relay-ops`, `pwc` tokens and tenant tokens can't (403 / 403 / 401).
2. There's always at least one active super-admin, even under concurrent requests.
3. Nobody can disable, demote or reset-password themselves through these routes.
4. A disabled or reset operator's existing tokens stop working on the next request.
5. A temporary password appears exactly once, in the creating or resetting response. It isn't logged, audited or retrievable afterwards, and it must be changed at first login.
6. Every successful mutation writes exactly one audit row in the same transaction. Failed or refused mutations write none.
7. No role migration and no new roles (D-05). No schema change beyond H's `037`.
8. Responses never include `password_hash`, tokens or secrets.

## Tests

| Test | Kind | Asserts |
|------|------|---------|
| `TestOperatorRoutes_RoleMatrix` | HTTP | super-admin 2xx; relay-ops 403; `pwc` token 403; tenant JWT 401 |
| `TestCreateOperator_TempPasswordFlow` | DB | 201 returns a temp password once; login → `password_change_required`; after the change, relay-ops can read relays but gets 403 on tenants and users |
| `TestCreateOperator_Duplicates409` | DB | active → `already_exists`; disabled → `exists_disabled`; email case-insensitive |
| `TestChangeRole_BumpsGenerationAndAudits` | DB | old token 401; one `provider_user.role_change` row with from/to |
| `TestDisable_EndsSessions_Enable_Restores` | DB | token 401 after disable; after enable, a new login works and the old token stays invalid |
| `TestResetPassword_Flow` | DB | the old password no longer works; old tokens 401; the temp password forces a change; audit has no password |
| `TestGuard_CannotModifySelf` | DB | self-disable, self-demote and self-reset → 409 |
| `TestGuard_LastSuperAdmin` | DB | demoting or disabling the only active super-admin → 409 |
| `TestGuard_LastSuperAdmin_Concurrent` | DB | two super-admins demote each other concurrently → exactly one succeeds |
| `TestOperatorMutations_AuditOnlyOnSuccess` | DB | refused and failed calls write no audit row |
| `TestOperatorResponses_NoSecrets` | HTTP | no `password_hash` anywhere; `temporary_password` only on create/reset, with `Cache-Control: no-store` |

DB-backed tests use the throwaway-DB harness. **A skipped DB test fails acceptance.**

## Acceptance Criteria

- [ ] AT-U.1 … AT-U.8 in [[Sprint21/Acceptance-Test-Plan]] pass.

## Build Check

```bash
cd controller && go build ./... && go vet ./... && go test ./internal/provider/... ./internal/middleware/...
```

## Implementation Checklist

- [ ] U1 store functions (transactional, generation bump, audit)
- [ ] U2 guards: self, last super-admin (locked), role values
- [ ] U3 handlers + routes; temp-password generation + hashing in the handler
- [ ] U4 audit actions
- [ ] U5 tests; build gate
- [ ] Tell M1 the API is merged (it unblocks M1-C5)

## Post-Phase Fixes

_None yet._
