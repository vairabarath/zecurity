---
type: phase
sprint: 23
owner: Sathiya (Person 2, M1)
features: [F7 suspend/resume and deletion, F12 quotas]
phases: [S1, S3, S4]
decisions: [D-45, D-47, D-46]
status: signed off 2026-11-17; ready for implementation
---

# Sprint 23 — Sathiya: S1 Suspend/resume (F7), S3 Quotas (F12), S4 Delete (F7)

Read `Sprint23/path.md` first (order, conflict zones, rules). Binding decisions: Decision Record amendment **2026-11-17** — **D-45** (S1), **D-47** (S3), **D-46** (S4). Also binding:
- **D-06:** tenant actions are super-admin only.
- **D-07:** what suspension blocks, as amended by D-45.
- **D-08:** agents stay connected and get a deny-all ACL. **Preserved.**
- **D-09, D-10, D-13:** deletion, the 30-day grace, the privileged purge.
- **D-11:** export, then destroy, as amended by D-46.
- **D-12:** append-only audit.
- **D-14:** provider actions are audited in `provider_audit_logs` only.
- **D-17:** a reason plus the typed slug, now extended to tenants.
- **D-34:** the protected predicates stay unchanged.

Order: **S1 → S3** (after Barath's **B1** is merged) **→ S4** (after S1). S2 (tenant creation) is Barath's.

---

## S1 — Suspend and resume (F7) — D-45

### What exists
- `workspaces.status` CHECK allows `active`, `suspended`, `deleted`, but **nothing sets them**.
- **Protected `status='active'` checks, which stay unchanged (D-34):**
  - `internal/middleware/workspace.go` (`WorkspaceGuard`);
  - `internal/connector/enrollment.go` (step 7);
  - `internal/connector/disconnect_watcher.go`;
  - `internal/shield/heartbeat.go`;
  - the SPIFFE validator.
- **ACL snapshots:** compiled in `internal/policy/compiler.go` and pushed on the control stream (`internal/connector/control_stream.go`). The connector fails closed on a deny-all (cancels sessions, refuses new ones).
- **Client sessions:** device session (`056_client_device_session`) and access tokens (`internal/client`, `internal/auth`).
- **User suspension** (SCIM, `internal/scim/directory_service.go`) is a different thing: users, not tenants.

### Build
- **Schema `064_tenant_suspension.sql`:** `workspaces.suspended_at TIMESTAMPTZ NULL`, `suspended_by TEXT NULL`, `suspension_reason TEXT NULL`. `status` stays `'active'`.
- **One authoritative eligibility check** (e.g. `internal/tenantstate.Eligibility(ctx, q, workspaceID)`): the only definition of "may this tenant act now". A lookup error means **ineligible** (fail closed).
  - **Refused while suspended:**
    - tenant admin login and client login;
    - **token refresh** (admin and client);
    - new client sessions;
    - configuration mutations (GraphQL and REST, next to `WorkspaceGuard`, not inside its status query);
    - enrollment of **new** connectors, shields and client devices.
  - **Revoked at suspend, in the same transaction or right after commit:** existing client sessions and access tokens, and tenant admin sessions. Use the existing revocation and generation mechanisms; no new token type.
  - **Agents (D-08 preserved):**
    - existing connectors and shields **keep their certificates, renewals and control streams**;
    - nothing here refuses or invalidates their authentication;
    - the ACL compiler returns a **deny-all** snapshot for a suspended workspace, pushed at once (`NotifyPolicyChange(workspace_id)`).
  - **Relay pools:** a suspended workspace gets an **empty relay list** and is **removed from residency relay allowlists** (D-40: bump the pool policy versions in the same transaction).
- **Resume:** pushes the normal ACL and relay list and restores the allowlists. Agents need no re-enrollment; users sign in again.
- **Provider API:**
  - `POST /provider/tenants/{id}/suspend` `{reason, confirm_slug}`; `POST /provider/tenants/{id}/resume` `{reason}`;
  - actions `tenant.suspend` / `tenant.resume` (super-admin, D-06);
  - audited with before/after (F17);
  - error codes `already_suspended`, `not_suspended`, `confirmation_mismatch`, `reason_required`;
  - tenant detail gets `suspension {suspended_at, by, reason}`; the tenants list shows the state.
- **Provider console:**
  - a tenant detail **Suspension** section;
  - a Suspend dialog (reason plus typed slug, D-17 extended) and Resume (reason);
  - a list badge;
  - `api-surface.test.ts`.
- **Admin UI and client:** a clear "this workspace is suspended by the provider" message on login and refresh refusal, instead of a generic error.

### Tests
- **Each refused path:** login, refresh, new session, each mutation family, and each new enrollment kind.
- **Revocation:** existing client sessions and admin sessions are revoked on suspend.
- **D-08:** existing agents' heartbeats, renewals and control streams continue; their snapshot is deny-all; after resume the normal ACL comes back with no re-enrollment.
- **D-34:** `status` stays `'active'`, and the protected predicates are untouched.
- **Relay pools:** removed from the allowlist and given an empty list, both restored on resume.
- **Fail closed:** a lookup error denies.
- **Authz and audit.**
- **Mutation checks:** remove the check from one path → its test fails; refuse agents → the D-08 test fails.

### Live check
Suspend a lab tenant:
- admin UI and client login refused, refresh refused;
- client traffic ends within seconds (probes);
- the connector stays connected with deny-all in its log;
- a new connector enrollment is refused;
- a residency relay drops the workspace.

Then resume: traffic returns with no re-enrollment.

---

## S3 — Quotas (F12) — D-47

**Needs Barath's B1 merged** (plans, `EffectiveLimits`). Rebase on S2 if it's merged, and cover its new creation paths.

### Build
- **Schema `065_tenant_quotas.sql`:** `workspace_quota_overrides (workspace_id, key, limit BIGINT NULL, reason, set_by, set_at)`, with the **six keys** of D-47: `users`, `resources`, `remote_networks`, `connectors`, `shields`, `client_devices`. A `NULL` override means explicitly unlimited; no row means "use the plan value".
- **`EffectiveLimits`** (from B1) = the plan's six values overlaid with the overrides. `nil` = unlimited; a missing key fails closed.
- **Free plan values** (seeded by B1, configurable): users 5, resources 50, remote_networks 3, connectors 10, shields 10, client_devices 25. **Legacy** is unlimited.
- **Counting (D-47):**

| Resource | Counts | Doesn't count |
|---|---|---|
| Users | active members, **pending invitations**, SCIM-suspended users (each identity once) | removed members, expired or revoked invitations |
| Resources | **every defined resource in any state except `deleting`** (un-protecting doesn't free a slot) | `deleting`; **discovered services** (`shield_discovered_services`, `connector_scan_results`) |
| Remote networks | every provisioned network, including unavailable ones | deleted |
| Connectors, shields | pending (token issued), active, inactive | revoked, deleted |
| Client devices | enrolled, not revoked | revoked, historical records |

  A suspended tenant's counts are unchanged.
- **Enforcement on every creation path**, server-side and **atomic with the creation**: inside the creating transaction, serialised per workspace (e.g. `SELECT … FROM workspaces WHERE id = $1 FOR UPDATE` before counting).
  - Remote networks: `CreateRemoteNetwork` (`graph/resolvers/connector.resolvers.go:24`).
  - Resources: `CreateResource` (`graph/resolvers/resource.resolvers.go:21`). **Discovery is never limited**: scans and discovered services keep working at the limit; only creating (adopting) a resource is refused.
  - Connectors: `GenerateConnectorToken` (`graph/resolvers/connector.resolvers.go`) and REST token regeneration if it creates a slot.
  - Shields: `GenerateShieldToken` (`graph/resolvers/shield.resolvers.go`).
  - Users:
    - `CreateInvitation` (`graph/resolvers/client.resolvers.go`, `internal/invitation`);
    - invitation accept;
    - SCIM user provisioning (`internal/scim`);
    - first sign-in / invited join (`internal/bootstrap`);
    - S2's first-admin invitation.
  - Client devices: enrollment (`internal/client/store.go`, `INSERT INTO client_devices`).
- **Over the limit:** refused with `quota_exceeded {key, limit, used}` (GraphQL error extension, REST JSON, SCIM error per RFC 7644).
- **Lowered limits:** **never delete existing resources** when a limit is lowered; such a tenant reads `over` and can't create more.
- **Fail closed:** a lookup error refuses the creation.
- **Provider API:** `PUT /provider/tenants/{id}/quotas/{key}` `{limit | null, reason}`; action `tenant.quota.set` (super-admin); audited with before/after. Tenant detail gets `quotas [{key, limit, source: plan|override|unlimited, used, over}]`.
- **Provider console:** a tenant detail **Quotas** section (usage vs limit, over-quota badge, override dialog with reason); `api-surface.test.ts`.
- **Admin UI:** the limits and usage (read-only), and the `quota_exceeded` message at each creation point.
- **Sessions:** not enforced (D-47).

### Tests
- **Every creation path:** refuses at the limit, allows below it, and creates nothing when refused.
- **Concurrency:** N parallel creations with one slot left → exactly one succeeds (DB-backed; mutation check by removing the lock).
- **Pending invitations count:** mass-inviting can't get around the limit; an identity that is both a member and invited counts once.
- **Resources:** un-protecting a resource doesn't free a slot; resource discovery keeps working at the resource limit.
- **Remote networks and resources:** creation is refused at their limits.
- **Lowered limits:** resources are kept and `over` is reported.
- **Unset limits** are unlimited; a plan change takes effect immediately.
- **Fail closed** on a lookup error.
- **Authz and audit** with before/after.

### Implementation status (2026-11-18)
Built on the local branch `feat/s3-tenant-quotas` (from `main` 28694918, B1 merged). **Not committed**: commit, push, PR and merge wait for the product owner's go. S2 merges first; then S3 rebases and adds the check to S2's first-admin invitation. Live check not run yet.
- **Schema** `065_tenant_quotas.sql`: `workspace_quota_overrides (workspace_id, key, limit_value, reason, set_by, set_at)`. The column is `limit_value` because `LIMIT` is a reserved word. A NULL limit is explicitly unlimited; no row means the plan's value.
- **`internal/quota`** (pgx only, so `providerquery` can use it, D-04) is the one definition:
  - `Effective`: plan values overlaid with the overrides; a missing key fails closed. `plans.EffectiveLimits` delegates to it, and `plans.Keys` aliases `quota.Keys`.
  - `Count` / `AllUsage` (one statement): the D-47 counting rules.
  - `Reserve(key)` / `ReserveUser(email)`: `SELECT … FROM workspaces … FOR UPDATE`, then count, then `*Exceeded {key, limit, used}`. An identity already counted (a user, or a pending unexpired invitation, by lowercased email) takes no new slot.
  - `SetOverride`.
- **Checked inside the creating transaction:**
  - `CreateRemoteNetwork`, `GenerateConnectorToken`, `GenerateShieldToken` (now transactional);
  - `resource.Create`, which also covers discovery adoption (discovery itself is never limited);
  - users: `invitation.CreateInvitation` and `AcceptInvitation`, `bootstrap` (invited join and first signup), SCIM `scimProvisioner`, client JIT `upsertUser`;
  - client devices: `insertClientDevice` (now transactional).
  - REST token regeneration inserts no row, so it takes no slot.
- **Refusals:**
  - GraphQL: the presenter sets `extensions {code: QUOTA_EXCEEDED, key, limit, used}`;
  - REST invitations: 409 `{"error":"quota_exceeded", key, limit, used, message}`;
  - SCIM: 403 with the quota text as detail (RFC 7644 has no quota scimType);
  - client gRPC: `ResourceExhausted`;
  - admin sign-in: `/login?error=user_limit_reached`.
- **Provider API:** `PUT /provider/tenants/{id}/quotas/{key}` takes `{limit: n|null, reason}` or `{inherit: true, reason}` (removes the override). The action is `tenant.quota.set` (super-admin only), audited with before/after `{limit, source}`. The tenant detail DTO gets `quotas [{key, limit, source, used, over}]` (one more statement: 7).
- **Provider console:** Quotas section (used, limit, source, "Over quota" badge, a Change dialog with a below-usage warning); `api-surface.test.ts`.
- **Admin UI:** GraphQL `workspaceQuotas` (CONSOLE_READ) and a Settings → Limits card (read-only); a "Limit reached" toast for `QUOTA_EXCEEDED`; the login message `user_limit_reached`.
- **Gates:**
  - controller: build, vet, full `go test -count=1 ./...` with the DB env (only the two known skips);
  - provider console: 627 tests, lint, build;
  - admin: 347 tests, build; lint shows the 17 problems already on `main`, none in S3 files.
- **Mutation checks:** all 18 caught: the lock (race), every creation path, the counting rules (deleting, identity once, expired invitations), off-by-one at the limit, the presenter, and relay-ops authz.
- **Test fixture changes:**
  - The SCIM test `seedWorkspace` now puts its workspace on the Legacy plan. A new workspace gets the default Free plan (5 users), and `TestGroups_Integration` provisions more than 5.
  - `schema_authz_test.go` registers `workspaceQuotas` and marks `WorkspaceQuota.key` as read-safe.
  - The tenant detail statement counts went from 6 to 7.


### Live check
- On the Free plan, create up to 3 remote networks; the 4th is refused. Create resources up to the limit (use a test plan with a small resource limit to keep it quick); discovery still runs.
- Plan limit 2 connectors; override 3 for one tenant.
- Create up to the limit; the next is refused in the admin UI and over the API with `quota_exceeded`.
- SCIM provisioning over the user limit is refused.
- Pending invitations count.
- Lower the limit below usage → "over quota", nothing removed.

---

## S4 — Delete: schedule, cancel, purge (F7) — D-46

**Needs S1 merged** (only suspended tenants can be deleted, D-10).

### Build
- **Schema `066_tenant_deletion.sql`:**
  - on `workspaces`: `delete_scheduled_at`, `delete_after`, `delete_reason`, `delete_by`, `legal_hold`, `legal_hold_reason`;
  - `workspace_tombstones` (id, slug, actor, scheduled_at, destroyed_at);
  - **purge progress** (`workspace_purge_steps` or a state column), so an interrupted purge can resume;
  - the **privileged purge role/function (D-13):** the only path that may delete one tenant's `audit_logs`, and only after export. `provider_audit_logs` is never deletable (D-12).
- **Provider API:**
  - `POST /provider/tenants/{id}/delete` `{reason, confirm_slug}`: only when **suspended** (`not_suspended` otherwise); sets `delete_after = now + 30 days` (D-10);
  - `POST /provider/tenants/{id}/delete/cancel` `{reason}`;
  - `PUT /provider/tenants/{id}/legal-hold` `{on, reason}`;
  - actions `tenant.delete`, `tenant.delete.cancel`, `tenant.legal_hold.set` (super-admin); all audited with before/after;
  - resume is refused while a deletion is scheduled (`delete_scheduled`): cancel first.
- **Purge job** (single controller, D-02; on the existing periodic-loop pattern), for tenants past `delete_after` and not under legal hold. Each step is **idempotent**, records its completion, and is safe to re-run:
  1. **Export** the tenant's `audit_logs` **and its security records**: certificate issuance and revocation history, credential and enrollment events (D-11 amended). Write them to the configured export location, and verify before continuing.
  2. **Destroy, everything tenant-scoped:**
     - credentials and sessions;
     - connectors, shields and client devices (revoked first, so the CRLs carry them);
     - relay authorization (allowlists, placement);
     - quota overrides;
     - plan assignment;
     - **key material** (workspace CA keys);
     - all dependent tenant data.
  3. Write the **tombstone**, and a `provider_audit_logs` row by a system actor.
- **Testing grace:** a **test-only setting** shortens the grace (refused outside development).
- **Backups:** data in DB backups persists until those backups expire. The backup retention and legal/security retention exceptions are an **ops/legal policy (open item)**; S4 only implements the legal hold.
- **Provider console:** a Deletion section (schedule with typed slug, countdown, cancel, legal hold); `api-surface.test.ts`.

### Tests
- Delete is refused unless the tenant is suspended; cancel works within the grace; resume is refused while scheduled.
- A legal hold pauses the purge.
- The purge exports, verifies, destroys and tombstones, in that order.
- **Crash mid-purge** at each step → a re-run completes with no double export and no partial state.
- `provider_audit_logs` is untouched; the keys are gone; the agents' certificates are on the CRL.
- **Mutation checks:** delete without suspension; purge without export.

### Live check
On a throwaway lab tenant:
1. Suspend → schedule delete → cancel → schedule again.
2. Legal hold on (purge paused) → legal hold off.
3. Purge with the test grace: the audit and security export exists; the tenant's data and CA keys are gone; the tombstone is there; the provider audit is intact.
4. Interrupt a purge (kill the controller mid-run), restart: it completes.

---

## Post-Phase Fixes

### Fix: S3 user slots keyed on email (quota bypass)
**Issue:** a user record created with an email the tenant already had took no user slot. A second SCIM or IdP account with a new subject but the same email, or many of them, got past the user limit. Separately, SCIM `Reactivate` set a removed (`deleted`) user back to active with no quota check.

**Root cause:** `quota.ReserveUser` skipped the check for any "counted identity" (an email held by a live user or a pending invitation), and the count de-duplicated by email.

**Fix applied** (`controller/internal/quota/quota.go`, `internal/invitation/store.go`, `internal/scim/directory_service.go`):
- **Count:** every live user row counts on its own; a pending, unexpired invitation counts once per email, and only while no live user has that email.
- **`ReserveUser`** (a user row is created or reactivated): free only when it uses the person's own pending invitation; otherwise it takes a slot.
- **`ReserveInvitation`** (new; invitation create and accept, no row created): free when the person is already counted.
- **SCIM `Reactivate`:** when the user was `deleted`, it runs `ReserveUser` in the transaction (403 at the limit); the UPDATE is conditional on the status it read.

**Tests:** a second account with the same email takes a slot; reactivating a removed user at the limit is refused; lifting a suspension isn't. Mutation checks (email-only skip, no reactivation check, invitation of a member counted twice) are all caught.

---

## Done when
S1, S3 and S4 are merged and live-checked; F12 is ticked; F7 is ticked together with Barath's S2.
