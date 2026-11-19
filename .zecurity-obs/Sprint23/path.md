---
type: sprint
sprint: 23
title: Provider Dashboard — remaining Must-have features (F5, F7, F8, F12)
status: decided and signed off (2026-11-17); implementation in progress
created: 2026-11-17
team:
  - Person 1 = Barath (M2): F8 plans (B1), F5 headroom (B2)
  - Person 2 = Sathiya (M1): F7 suspend/resume (S1), F7 tenant creation (S2, handed over 2026-11-18), F12 quotas (S3), F7 deletion (S4)
baseline: main 562c97e1 (relay pools F4/F9/F10/F11 done; Must have 14 / 18)
---

# Sprint 23 — Remaining Must-have features

## Goal

Finish the provider dashboard **Must-have** list (Phase 1, F1–F18). Four features are left:

| Feature | Today | Phases (owner) |
|---|---|---|
| **F5** Capacity view — sessions, bandwidth, **headroom** | ◐ sessions and bandwidth done (PR #193) | **B2** (Barath) |
| **F8** Tenant detail — counts, **plan** | ◐ counts done (Sprint 21, PR #190) | **B1** (Barath) |
| **F7** Tenant **create / suspend / delete** | ⬜ | **S2** create, **S1** suspend/resume and **S4** delete (Sathiya; S2 handed over by Barath) |
| **F12** Per-tenant **quotas** | ⬜ | **S3** (Sathiya) |

When all four are merged **and** live-checked, the Must-have list is complete (18 / 18).

Tracker: `.zecurity-obs/Planning/Provider-Dashboard-Features.md`. **Binding decisions:** `docs/provider-dashboard-architecture-decisions.md` → **amendment 2026-11-17 (D-42 … D-47)**, on top of D-01 … D-41.

---

## Decisions (Phase 0 — done 2026-11-17)

Decided by the product owner (Barath) after review; the full text is in the Decision Record. Summary:

| ID | Decision | Phase |
|---|---|---|
| **D-42** | F5: **operator-configured link capacity** per relay (bytes/s); one capacity per relay (its tighter direction); headroom = capacity − (to clients + to connectors), since both flows share the relay's link; rate, capacity and headroom shown separately; unset = **"Not configured"**; an operational estimate. No relay/proto change. | B2 |
| **D-43** | F8: **provider-managed plans** with limits for **all six quota keys** (explicit `null` = unlimited; a missing key is invalid and fails closed); one default. Seeded: **Free** (default for every new workspace): users 5, resources 50, remote_networks 3, connectors 10, shields 10, client_devices 25; **Legacy** (all unlimited, operator-assigned only, never advertised). Values are configurable plan data. Every workspace gets the **current** default on insert (`BEFORE INSERT` trigger when none is given). No pricing/billing; feature entitlements are a later decision. Changes take effect immediately; lowering limits never deletes resources; no deleting an assigned or default plan. | B1 |
| **D-44** | F7: super-admins **create a workspace** (same bootstrap path as self-signup) and **invite its first admin** (existing single-use, time-limited invitation, accepted by the matching verified email; admin role). Self-signup stays. | S2 |
| **D-45** | F7: **suspension** = `suspended_at`, `suspended_by`, `suspension_reason`; `status` stays `'active'`. **One eligibility check**: refuses login, refresh, new sessions, configuration mutations and **new** enrollments; **revokes** client sessions/tokens and admin sessions; **agents keep their certificates and connections and get deny-all (D-08 preserved)**; empty relay list and removal from residency allowlists. Resume pushes the normal ACL. Reason + typed slug. | S1 |
| **D-46** | F7: **deletion** of a suspended tenant only; 30-day grace, cancellable; **idempotent, resumable purge**; export `audit_logs` **and security records**, destroy everything tenant-scoped incl. keys, keep a tombstone; privileged purge role (D-13); **legal hold** pauses the purge; backup retention is an ops/legal policy (open, not blocking). | S4 |
| **D-47** | F12: **hard limits on six keys** (users, resources, remote_networks, connectors, shields, client_devices), tenant-wide, no session quota; plan value + per-tenant override; **server-side at every creation path, atomic with the creation**; `quota_exceeded {key, limit, used}`; counting rules (below); discovery is never limited. | S3 |

**Amended:** D-07 (by D-45), D-11 (by D-46), D-17 (extended to tenant suspend/delete), D-30 (new console writes), D-31 (relay-ops sets link capacity). **Preserved:** D-08. **Implemented unchanged:** D-09, D-10, D-13. **Consistent, not amended:** D-34. **Unchanged:** D-06, D-12, D-14.

**Quota counting (D-47):**

| Resource | Counts | Doesn't count |
|---|---|---|
| Users | active members, **pending invitations**, SCIM-suspended users (each identity once) | removed members, expired or revoked invitations |
| Resources | **every defined resource in any state except `deleting`** (un-protecting doesn't free a slot) | `deleting`; **discovered services** (`shield_discovered_services`, `connector_scan_results`) — discovery keeps working at the limit |
| Remote networks | every provisioned network, including unavailable ones | deleted |
| Connectors, shields | pending (token issued), active, inactive | revoked, deleted |
| Client devices | enrolled, not revoked | revoked, historical records |

A suspended tenant's counts are unchanged.

**Open (not blocking):** backup retention period and legal/security retention exceptions (D-46) — product owner, recorded separately.

---

## Team assignments

**Person 1 — Barath (M2):** B1 plans → B2 headroom. Phase file: `Member1-Barath/Phases-B1-B2-Plans-and-Headroom.md` (also holds the S2 record).
**Person 2 — Sathiya (M1):** S1 suspend/resume → S2 tenant creation → S3 quotas → S4 deletion. Phase file: `Member2-Sathiya/Phases-S1-S4-Tenant-Lifecycle-and-Quotas.md`.

**2026-11-18:** Barath handed **S2** to Sathiya (branch `feat/s2-tenant-creation`; record in Barath's phase file, S2 section). It still merges before S3; migration `067_tenant_first_admin_invite` is used.

Both: review each other's PRs; the sprint acceptance run (live, on a reset lab DB, current `main`).

### Phases and order

| Phase | Owner | Feature | Depends on | Migration |
|---|---|---|---|---|
| **B1** Plans | Barath | F8 | D-43 | `062_tenant_plans.sql` |
| **B2** Relay link capacity + headroom | Barath | F5 | D-42 | `063_relay_link_capacity.sql` |
| **S2** Provider tenant creation | Sathiya (handed over by Barath) | F7 | D-44, **B1 merged** | `067_tenant_first_admin_invite.sql` |
| **S1** Suspend / resume | Sathiya | F7 | D-45 | `064_tenant_suspension.sql` |
| **S3** Quotas | Sathiya | F12 | D-47, **B1 merged** | `065_tenant_quotas.sql` |
| **S4** Delete: schedule, cancel, purge | Sathiya | F7 | D-46, **S1 merged** | `066_tenant_deletion.sql` |

```
Barath:  B1 (plans) ──► B2 (headroom)
           │
           └──── B1 merged ────► S2 (create) ──► S3 (quotas)   (S2 and S3 share user-creation files: S2 merged first)
Sathiya: S1 (suspend) ──► S2 ──► S3 ──► S4 (delete, needs S1)
```

- **Critical path:** B1 lands first. Sathiya starts with S1 while B1 is in progress; **S2 and S3 don't start implementation before B1 is merged**.
- Migration numbers checked against `main` (latest `061_mfa_step_up.sql`). Numbers are reserved per phase, not per merge order; a phase that merges earlier keeps its number. Never edit an existing migration.

### Conflict zones (shared files)

| File / area | Who | Rule |
|---|---|---|
| `controller/cmd/server/main.go` (routes, wiring) | both | Own block per phase; rebase before merge. |
| `controller/internal/provider/authz.go` (`decide()`) | both | New actions: `plan.*`, `tenant.plan.assign` (B1); `relay.capacity.set` (B2); `tenant.create` (S2); `tenant.suspend`, `tenant.resume` (S1); `tenant.quota.set` (S3); `tenant.delete`, `tenant.delete.cancel`, `tenant.legal_hold.set` (S4). `tenant.*` and `plan.*` super-admin only (D-06); `relay.capacity.set` relay-ops and super-admin (D-31 amended). |
| `internal/providerquery/tenants.go`, tenant detail DTO | both | B1 `plan`; S1 `suspension`; S3 `quotas`; S4 `deletion`. Update `TestTenantDTOSurface` in the same PR. |
| **User-creation paths** (`internal/invitation`, `internal/bootstrap`, `graph/resolvers/client.resolvers.go`, `internal/scim`) | Barath (S2), Sathiya (S3) | **S2 merges first**; S3 rebases and adds the quota check to S2's new paths too. |
| `provider-console/src/pages/TenantDetail*`, `Tenants*` | both | One section per feature (Plan, Suspension, Quotas, Deletion); create dialog on the Tenants page. |
| `provider-console/src/api-surface.test.ts` | both | Every new console write added on purpose (D-30 amended). |
| `controller/migrations/` | both | Reserved numbers above; `[schema: reset DB]` in the PR title. |

---

## Rules (this sprint)

- Decisions **D-42 … D-47 are binding**. If a phase needs another architectural choice, stop and ask; don't decide it in code.
- **D-08 holds:** nothing in S1 may refuse or invalidate an existing connector's or shield's authentication; suspended agents get deny-all.
- **D-34 holds:** the protected `workspaces.status='active'` predicates (SPIFFE validator, disconnect watchers, enrollment, `WorkspaceGuard`'s status query) stay unchanged.
- **Every new console write**: server-side authorization in `decide()`; a **reason** for tenant-affecting and destructive actions (plan assignment, suspend, resume, delete, cancel, quota override, legal hold); a **typed tenant slug** for suspend and delete (D-17 extended); audit with before/after (F17); an entry in `api-surface.test.ts`. Never log or audit secrets (invitation tokens, keys).
- **Fail closed:** an eligibility lookup error denies; a quota lookup error refuses the creation.
- **Quota checks are atomic with the creation** (same transaction, serialised per workspace) and enforced on every server-side path, not only the consoles.
- Schema: new numbered files only, no migration framework, recreate the local DB (`docs/database-development.md`).
- Each phase ends with a **live check** recorded in `.zecurity-obs/Planning/Pending-Live-Tests.md` (results log) before its tracker box is ticked.
- **No commit, push, PR or merge without the product owner's explicit go** for that step.

## Test gates (each phase)

- **Controller:** `go build ./... && go vet ./...`, then the full `go test ./...` with the DB env: 0 failures, and only the two known hard-coded skips.
- **Provider console:** `npm test`, `npm run lint`, `npm run build`. Do the same for the **admin UI** when it's touched (S1 suspended message, S3 quota errors, S2 invitation acceptance).
- **Mutation checks** for each new guard:
  - the quota race;
  - the eligibility check on each refused path;
  - D-08 (agents unaffected);
  - delete only when suspended;
  - purge resume after an interruption;
  - plan delete refused while assigned.
- Phase-specific tests are listed in the phase files.

## Acceptance (end of sprint)

Live run on a **reset lab DB with current `main`**. Record the Git SHA and latest migration, use traffic probes where traffic is involved, and keep the logs.

- **F5:**
  - set a relay's link capacity (relay-ops), with an audit row;
  - push known traffic: rate, capacity and headroom per direction appear separately on Relays, relay detail and Overview;
  - an unset capacity shows "Not configured";
  - a rate above capacity shows headroom 0 and "over capacity".
- **F8:**
  - create and edit plans and set the default;
  - assign a plan to a tenant (reason, audited);
  - tenant detail shows the plan and its limits;
  - the seeded **Free** (default) and **Legacy** plans exist with their values; a new signup gets **Free**; after switching the default, a new signup gets the new default;
  - a plan without all six keys is refused;
  - deleting an assigned plan and deleting the default plan are refused.
- **F7 create:** create a tenant in the console; the invited first admin (matching email) accepts and signs in as admin; the invitation can't be reused.
- **F7 suspend/resume:**
  - suspend (reason + typed slug): admin and client login refused, refresh refused, client traffic ends within seconds, admin sessions revoked;
  - connectors and shields stay connected with deny-all (connector log); a new enrollment is refused; the tenant leaves residency allowlists;
  - resume: traffic returns with **no re-enrollment**.
- **F7 delete:**
  - schedule on a suspended tenant; cancel; schedule again; deleting an active tenant is refused;
  - the purge with a test grace exports audit and security records, destroys data and keys, and leaves a tombstone;
  - an interrupted purge resumes;
  - a legal hold pauses the purge;
  - `provider_audit_logs` stays intact.
- **F12:**
  - plan limits plus an override;
  - every creation path is refused at the limit with `quota_exceeded` (admin UI, API, SCIM);
  - concurrent creations can't pass the last slot;
  - pending invitations count; un-protecting a resource doesn't free a slot; resource discovery keeps working at the resource limit;
  - remote network and resource creation are refused at their limits;
  - lowering a limit keeps resources and shows "over quota".

Tracker: F5, F7, F8, F12 ticked only after their live checks → **Must have 18 / 18**.

## Checklist

- [x] Phase 0: D-42 … D-47 decided and written into the Decision Record (2026-11-17)
- [x] Documents reviewed (Sprint 23 files, Decision Record amendment, CLAUDE.md) — signed off by Barath 2026-11-17; implementation may start
- [x] B1 — Plans (Barath) — merged (#222) — live-checked 2026-11-19 (PASS)
- [x] B2 — Relay link capacity + headroom (Barath) — merged (#226) — live-checked 2026-11-19 (PASS)
- [x] S2 — Provider tenant creation (Sathiya, handed over by Barath) — merged (#225) — live-checked (branch 2026-11-18; recheck on `main` 2026-11-19, PASS)
- [x] S1 — Suspend / resume (Sathiya) — merged (#223) — live-checked 2026-11-19 (PASS; residency allowlist part not run, one relay)
- [x] S3 — Quotas (Sathiya) — merged (#224) — live-checked 2026-11-19 (PASS)
- [ ] S4 — Delete: schedule, cancel, purge (Sathiya) — merged (#229, 2026-11-19) — live-checked on the branch 2026-11-18/19 (local export and S3 export with real agents, PASS)
- [ ] Sprint acceptance run (both); tracker 18 / 18 — run 2026-11-19 on `main` `4d1faf50`: B1, B2, S1, S2 recheck, S3 and cross-feature PASS (PR #230); S4 check on `main` still to do (reset DB for `066_`); tracker 17 / 18

## Post-Sprint Fixes

- **S1 (PR #223 review, 2026-11-18):** an invited join into a suspended tenant is refused before any write; refresh reports "suspended" ahead of the generation check. Details: Sathiya's phase file, "S1 Post-Phase Fixes".
- **S2: expired first-admin invitation (2026-11-18).** Sign-in is now refused with `invitation_expired` instead of self-signing up the invitee, and super-admins re-issue the invitation with `POST /provider/tenants/{id}/first-admin-invitation` (`tenant.first_admin.reinvite`), from the console's tenant-detail "First admin" card. Details: Barath's phase file, "S2 Post-Phase Fixes".
- **S2: Google sign-in invitation choice (2026-11-18).** With pending invitations in several workspaces, the first Google sign-in now joins, in order: the workspace it signed in for, then a provider first-admin seat, then the oldest invitation (it was arbitrary).
- **S3, user slots keyed on email (2026-11-18):** a second account with an email already in use took no slot, and SCIM reactivation of a removed user wasn't checked. Users now count per row (plus invitations without a user); `ReserveInvitation` is new; reactivation is checked. Details are in the S3 phase file, "Post-Phase Fixes".
- **S4: purge steps only through `SECURITY DEFINER` functions (security review, 2026-11-18).** The app role could write `tenant_purges` directly, so every step change now goes through the `tenant_purge_*` functions in `066_tenant_deletion.sql` (the app role only reads `tenant_purges` and `workspace_tombstones`), and `tenant_purge_start` locks the workspace so it can't race a cancel. Details: Sathiya's phase file, S4 "Security review".
- **S4: download links cut off by a legal hold (live check, 2026-11-18).** A legal hold kept past the grace made the emailed links invalid while the data still existed. Links now work until the purge **starts**; the "(in 1 days)" countdown was also fixed (`timeUntil`). Details: Sathiya's phase file, S4 "Live check".
- **S4: proposed amendment to D-45/D-46 (Sathiya, 2026-11-18, needs Barath's confirmation).** The deletion export is tenant-private: it goes to the tenant's own S3 audit-export sink if one is configured, else to `TENANT_PURGE_EXPORT_DIR`. Tenant admins download it during the grace through an emailed link or an export-only sign-in (single-use 15-minute token, no session). Both work until the purge starts. The provider dashboard never shows tenant content. Recorded in Sathiya's phase file and PR #229; not yet in the Decision Record.

## Out of scope

Billing and invoicing (F34), usage metering and session quotas (F32, F33), alerting (F26), more provider roles (F19), impersonation (F31), reseller hierarchy (F37), bulk tenant pool moves (F30), backup retention policy (D-46 open item). Connector relay-failure detection (O1/O3) was fixed separately in PR #218.
