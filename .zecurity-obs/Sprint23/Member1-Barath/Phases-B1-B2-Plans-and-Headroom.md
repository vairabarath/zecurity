---
type: phase
sprint: 23
owner: Barath (Person 1, M2)
features: [F8 plan, F5 headroom, F7 tenant creation]
phases: [B1, B2, S2]
decisions: [D-43, D-42, D-44]
status: signed off 2026-11-17; ready for implementation
---

# Sprint 23 — Barath: B1 Plans (F8), B2 Relay headroom (F5), S2 Tenant creation (F7)

Read `Sprint23/path.md` first (order, conflict zones, rules). Binding decisions: Decision Record amendment **2026-11-17** — **D-43** (B1), **D-42** (B2), **D-44** (S2); D-06, D-17 (extended), D-30 and D-31 (amended) apply.

Order: **B1 → B2 → S2**. B1 lands first because S2 and Sathiya's S3 read plans.

---

## B1 — Plans and the tenant's plan (F8) — D-43

### What exists
- **Tenant detail** (Sprint 21):
  - API: `GET /provider/tenants/{id}` → `providerquery.GetTenant` (`internal/providerquery/tenants.go`);
  - content: counts and policy counts (PR #190);
  - console: `provider-console/src/pages/TenantDetail*`.
- **Workspace creation:** `internal/bootstrap/bootstrap.go` (`INSERT INTO workspaces`, at first sign-in).
- No plan, price or limit exists anywhere.

### Build
- **Schema `062_tenant_plans.sql`:**
  - **`plans` table:**
    - `id`, `name` (unique), `description`;
    - `limits JSONB` with **all six keys** of D-47 (`users`, `resources`, `remote_networks`, `connectors`, `shields`, `client_devices`), each a non-negative integer or an **explicit `null` = unlimited**. A CHECK (or a validation function) rejects a plan without all six keys: **a missing key never means unlimited**;
    - `is_default`: exactly one row, enforced by a partial unique index;
    - `created_at`, `updated_at`.
  - **Seeds:**
    - **Free**, the default: users 5, resources 50, remote_networks 3, connectors 10, shields 10, client_devices 25;
    - **Legacy**: all six keys `null`, operator-assigned only, never advertised.
  - **`workspaces.plan_id`:** FK, `NOT NULL`, **no fixed column default** (it couldn't follow a changed default plan). Instead, a **`BEFORE INSERT` trigger** sets `plan_id` to the **current** default plan (`SELECT id FROM plans WHERE is_default`) when an insert doesn't give one. This also covers the ~70 workspace inserts in tests and fixtures without touching them.
  - Backfill any existing workspaces to **Legacy**. In practice there are none: the DB is recreated after every schema change, so this is only a safety net.
  - Grants for the runtime role, as in `053_` / `059_`.
- **Controller** (new `internal/plans` package, or under `internal/provider`; `providerquery` stays free of `net/http`, D-04):
  - **Reads:** `GET /provider/plans`, `GET /provider/plans/{id}`. Super-admin only (plans are commercial/tenant data, D-06).
  - **Plan writes:**
    - `POST /provider/plans`;
    - `PATCH /provider/plans/{id}` (name, description, limits);
    - `POST /provider/plans/{id}/default`;
    - `DELETE /provider/plans/{id}`: refused while any workspace has the plan (`plan_in_use`) or while it is the default (`plan_is_default`).
  - **Assignment:** `PUT /provider/tenants/{id}/plan` `{plan_id, reason}`. Plan changes and reassignment take effect **immediately** on effective limits.
  - Authorization and audit:
    - `decide()` actions `plan.create`, `plan.update`, `plan.delete`, `plan.set_default`, `tenant.plan.assign`, all super-admin only;
    - every write is audited with before/after (F17); assignment carries its reason.
  - Error codes: `invalid_limits`, `plan_name_taken`, `plan_not_found`.
  - **Shared interface for S3:** `EffectiveLimits(ctx, q, workspaceID) (map[string]*int64, error)`.
    - It returns the plan's six values, with a hook for S3's per-tenant overrides.
    - Agree the signature with Sathiya on day 1.
    - `nil` = explicitly unlimited; **a missing key returns an error** (fail closed).
  - **New workspaces get the current default plan** through the trigger. S2's provider create passes its chosen `plan_id` explicitly (the trigger only fills a missing one).
  - Tenant detail gains `plan {id, name, limits}`; update `TestTenantDTOSurface`.
  - A test asserts that new workspaces (self-signup and S2) get the **current** default, including after the default is switched.
- **Console:**
  - a **Plans** page: list, create, edit limits, set default, and delete with the refusals explained;
  - a tenant detail **Plan** section with a Change plan dialog (reason required);
  - `api-surface.test.ts` entries for every new write.
- **Never delete resources** when a limit is lowered (D-43). Blocking further creation is S3's job.

### Tests
- **DB:**
  - exactly one default plan: `set_default` swaps atomically;
  - delete is refused while the plan is the default or assigned;
  - assignment is audited with before/after and takes effect immediately in `EffectiveLimits`;
  - a new workspace gets the default plan;
  - the seeded Free and Legacy values;
  - limits validation: all six keys required, known keys only, non-negative integers or `null`;
  - `EffectiveLimits` fails closed on a missing key.
- **Authz:** every plan action is super-admin only; relay-ops get 403.
- **Console:** the Plans page, the change-plan dialog (reason required), and the api-surface entries.
- **Mutation checks:** the plan-in-use guard and the single-default invariant.

### Live check
- Check the seeded **Free** (default) and **Legacy** plans and their values.
- Create a plan and set it as the default; a new signup gets it. Switch back to Free.
- A plan without all six keys is refused.
- Assign a plan to a tenant (with a reason): tenant detail shows the plan and its limits, and the audit row has before/after.
- A new signup gets the default plan.
- Deleting an assigned plan and deleting the default plan are both refused.


### B1 implementation record (branch `feat/sprint23-b1-plans`)
- **Schema `062_tenant_plans.sql`:**
  - `plan_limits_valid(jsonb)`: exactly the six keys, each null or a whole number. It's used by a CHECK, so the database itself refuses a plan with a missing, extra, negative or fractional limit.
  - The `plans` table with a single-default partial unique index.
  - Seeds with fixed ids: **Free** `00000000-0000-0000-0001-000000000001` (the default: 5 / 50 / 3 / 10 / 10 / 25) and **Legacy** `…0001-000000000002` (all null).
  - `workspaces.plan_id` (FK `ON DELETE RESTRICT`, `NOT NULL`), backfilled to Legacy.
  - The `workspaces_default_plan` `BEFORE INSERT` trigger, which fills the current default.
  - A grant to `ztna_app`.
- **`internal/plans`** (`plans.go`, no `net/http`):
  - `Limits` with `Validate`.
  - Store `Create` / `Update` / `Delete` / `SetDefault` / `Assign`: each takes row locks and runs its audit hook in the same transaction. `SetDefault` locks **every plan row in id order**. A lock set that depends on the changing default ("the default plus the target") deadlocked under concurrency; this was found in the full suite; `Delete` locks the plan row, and the FK backs the in-use rule.
  - `Get`.
  - **`EffectiveLimits(ctx, q, workspaceID)`**, the S3 interface: `nil` = unlimited; an unknown workspace or a missing key returns an error (fail closed).
- **`internal/plans/handler.go`:** `POST /provider/plans`, `PATCH /provider/plans/{id}`, `DELETE /provider/plans/{id}`, `POST /provider/plans/{id}/default`, `PUT /provider/tenants/{id}/plan`.
  - Error codes: `invalid_limits`, `invalid_name`, `invalid_description`, `plan_name_taken`, `plan_in_use`, `plan_is_default`, `plan_not_found`, `reason_required`, `reason_too_long`, `forbidden`, `not_found`.
  - Audit rows carry before/after.
- **Authz:** actions `plan.read`, `plan.create`, `plan.update`, `plan.delete`, `plan.set_default`, `tenant.plan.assign` (super-admin only through `decide()`'s existing prefix rule); `CanReadPlans`, `CanManagePlans`.
- **Reads (`providerquery/plans.go`, `read_handlers.go`):** `GET /provider/plans` (default first, with a tenant count) and `GET /provider/plans/{id}`. Tenant detail gains `plan {id, name, limits, is_default}`: one more constant statement, so the statement-count pins moved from 5 to 6.
- **Console:**
  - a **Plans** page (super-admin nav entry under Fleet): list with the six limits and tenant count, Create, Edit, Make default, and Delete (disabled while tenants are on the plan);
  - `PlanDialogs.tsx`, where each limit is a whole number or "Unlimited";
  - a tenant detail **Plan** section with a Change plan dialog;
  - `api/plans.ts`, `listPlans` / `getPlan`, `lib/plan-format.ts`;
  - `api-surface.test.ts` with 5 writes and 2 reads added.
- **Choices beyond the doc:**
  - A **reason is also required** for plan update, delete and set-default, not only assignment, because each changes what tenants may create. Create needs none.
  - Deleting the default plan is refused with `plan_is_default`, and the console hides Delete and Make default on the default plan.
- **Tests:**
  - `internal/plans` (8):
    - seeds and the current-default trigger, including after switching the default and when an insert names a plan;
    - limits validation, in the app and the DB CHECK;
    - set-default, including **24 concurrent switches across 3 plans** → exactly one default and no deadlock (10/10 runs; the earlier lock set deadlocks reliably in this test);
    - delete guards;
    - assignment and edits taking effect at once in `EffectiveLimits`, which fails closed;
    - parsing a missing key → error;
    - HTTP: relay-ops 403 on every write with no state change and no audit; create/update/assign/delete/default with error codes and audit before/after.
  - `internal/provider`: plan reads (super-admin list and get, relay-ops 403, tenant detail plan).
  - `providerquery`: DTO pins for `TenantPlan`, `PlanSummary` and `plan` on `TenantDetail`.
  - Console: `Plans.test.tsx` (6), `TenantDetailPlan.test.tsx` (4); the tenant-detail button pin includes Change plan.
- **Gates (2026-11-17):** controller `go vet ./...` clean; full `go test ./...` 1,906 passed, 0 failed (2 known hard-coded skips). Provider console 621 tests, lint and build clean.
- **Live check:** pending (reset DB on this branch; migration `062_`).

---

## B2 — Relay link capacity and bandwidth headroom (F5) — D-42

### What exists
- **Bandwidth** (PR #193):
  - heartbeat fields 9/10, migration `052_relay_bandwidth.sql`;
  - `GET /provider/relays[/{id}]` returns `bandwidth` (bytes/s per direction, window, measured_at);
  - console: a Bandwidth column, a detail card, and Overview tiles.
- **Sessions:** `connection_count` / `max_connections` and the capacity tier are already shown.

### Build
- **Schema `063_relay_link_capacity.sql`:** `relays.link_capacity_bps BIGINT NULL CHECK (link_capacity_bps > 0)`, in **bytes per second**, consistent with F5.
- **Controller:**
  - `PUT /provider/relays/{id}/link-capacity` `{bps | null, reason}`:
    - action `relay.capacity.set` (relay-ops and super-admin, D-31 amended);
    - audited with before/after.
  - **Relay reads gain:**
    - `link_capacity_bps` (null = not configured);
    - `headroom {to_clients_bps, to_connectors_bps, over_capacity}`, where headroom per direction = capacity − measured rate, clamped at 0, with `over_capacity` true when the rate exceeds capacity;
    - `headroom` is **null when the capacity is unset, or when bandwidth is not reported or still being measured**. Never zero or unlimited by default.
- **Console:**
  - Show **measured rate, configured capacity and remaining headroom separately**:
    - in a Relays list Headroom column;
    - in the relay detail card, with a "Set link capacity" dialog (UI in Mbps/Gbps, API in bytes/s, reason required);
    - in an Overview fleet tile.
  - An unset capacity reads **"Not configured"**.
  - Label headroom as an **operational estimate**, not guaranteed available bandwidth.
  - Add an Overview attention item for relays at ≥ 90 % of capacity (a console constant).
- **No relay, proto or connector change.**

### Tests
- **Headroom maths:** per direction, the clamp, `over_capacity`, unset capacity, still measuring, and revoked/not reported → null.
- **Authz:** relay-ops and super-admin allowed, others refused.
- **Audit:** before/after rows.
- **Console:** formatting and units, the "Not configured" label, the estimate wording, and the attention threshold.

### Live check
- Set the capacity on a lab relay (as relay-ops).
- Push known traffic (as in the F5 run: `curl` a large file through the relay): rate, capacity and headroom per direction appear separately on the list, the detail and the Overview.
- A rate above capacity shows headroom 0 and "over capacity".
- Clear the capacity: it shows "Not configured".
- The capacity change is audited.

---

## S2 — Provider tenant creation (F7) — D-44

**Starts after B1 is merged** (a plan is set on create). Shares the user-creation files with Sathiya's S3: **S2 merges first**, then S3 adds quota checks to S2's paths too.

### What exists
- **Self-signup:** `internal/bootstrap/bootstrap.go` creates the workspace, trust domain, workspace CA and default policy at the first admin's sign-in. An invited-join path exists (`internal/bootstrap/invited_join_test.go`).
- **Invitations** (`internal/invitation/store.go`):
  - `pending` → `accepted` (single-use);
  - expire via `expires_at`;
  - accepted against the signed-in user's verified email;
  - today they join as a **member**.

### Build
- **API:** `POST /provider/tenants` `{slug, name, plan_id?, admin_email, reason}`.
  - Action `tenant.create` (super-admin, D-06); audited with the provider actor and the invitation operation, **never the invitation token**.
  - Error codes: `slug_taken`, `invalid_slug`, `invalid_email`, `plan_not_found`.
- **Workspace:** created through the **same bootstrap path** as self-signup (workspace, trust domain, workspace CA, default policy), with **no first user**. The plan is `plan_id`, or the default.
- **First admin:** gets a **single-use, time-limited invitation** that grants the **admin** role on accept. It can be accepted only by a signed-in user whose verified email matches.
  - If invitations can't carry a role today, add one: schema `067_` (reserved) plus a test that only the provider-issued first-admin invitation can grant admin.
- **Invariants:** self-signup stays unchanged, as do workspace isolation and onboarding.
- **Console:** a Tenants page "Create tenant" dialog (slug, name, plan, admin email, reason); `api-surface.test.ts`.

### Tests
- **Equivalence:** the created workspace matches a self-signup one (CA, default policy, trust domain, plan).
- **Slug race:** two creates with the same slug → one wins.
- **Invitation:**
  - single-use (a second accept is refused);
  - expires;
  - refused for a non-matching email;
  - grants admin only for the first-admin invitation.
- **Authz:** super-admin only.
- **Audit:** no token in the details.
- **Mutation checks:** the email binding and single-use.

### Live check
- Create a tenant in the console.
- The invited first admin (matching email) accepts and signs in to the admin UI as admin, and can enrol a connector.
- Reusing the invitation is refused.
- A non-matching account is refused.

### S2 implementation record (branch `feat/s2-tenant-creation`, built by Sathiya)
Barath handed S2 to Sathiya on 2026-11-18 (B2 not started). Built from `main` after B1 (#222), before S3 (#224), which rebases onto it.

- **Schema `067_tenant_first_admin_invite.sql`** (the reserved slot was needed):
  - `invitations.invited_by` is now nullable. NULL means issued by the provider; the operator is in `provider_audit_logs`, not here.
  - New `invitations.role` (`member` | `admin`, default `member`).
  - CHECK `invitations_admin_only_from_provider`: `role = 'member' OR invited_by IS NULL`. An invitation a tenant user sends can never grant admin.
  - CHECK `invitations_sender_required_for_member`: a sender-less invitation is always the admin one.
- **`internal/bootstrap`:**
  - `createWorkspaceTx` is the workspace half of bootstrap (workspace row with trust domain, workspace CA, `active`). Self-signup (`runBootstrapTransaction`) and `CreateTenant` both call it; self-signup is otherwise unchanged. The Default policy (049_), audit heads (053_) and plan (062_) come from the `workspaces` triggers on both paths.
  - `CreateTenant` (`tenant_create.go`) runs in one transaction:
    - validates the slug (one DNS label, ≤ 60 so `ws-<slug>` fits) and the name, and normalises the email;
    - locks the plan `FOR SHARE`;
    - creates the workspace with **no first user**;
    - writes the first-admin invitation (`role='admin'`, no sender, 7 days) and an `invited` membership row with role `admin`;
    - writes the tenant-stream `workspace.create` (`source: provider`) and the caller's provider audit.
  - A slug race is the UNIQUE index; the loser gets `ErrSlugTaken`.
  - **Invited join (`Provision`)** for a first-admin row:
    - usable only while its invitation is `pending` and unexpired (the expiry was not checked at sign-in before);
    - refused with `ErrNotInvited` if the IdP did not assert `email_verified`;
    - the transaction **consumes the invitation first** (`UPDATE … status='pending' AND expires_at > NOW() RETURNING`), then requires exactly one `invited` membership row, so a concurrent second sign-in is refused;
    - audited as `invitation.accept` (role admin, never the token).
  - Rows from invitations sent by tenant users behave exactly as before.
- **`internal/invitation`:** `Invitation.Role`; `GetByToken` reads a NULL sender as `""`. `AcceptInvitation` (`/api/invitations/{token}/accept`) accepts **member invitations only**; the admin seat is taken only at the invitee's first sign-in.
- **`internal/client`:** `checkClientInvitation` refuses an admin invitation (`FailedPrecondition`, "accept in the admin console"). Before this, a client CLI sign-in with the token would have consumed it as a member join. `markInvitationAccepted` only marks member invitations.
- **API `internal/tenantcreate`:** `POST /provider/tenants {slug, name, plan_id?, admin_email, reason}` → 201 `{id, slug, name, trust_domain, plan_id, plan_name, invitation {id, email, expires_at}}`.
  - Action `tenant.create` (`CanCreateTenant`, super-admin only through `decide()`).
  - Reason required (≤ 500).
  - Errors: `slug_taken` (409), `invalid_slug`, `invalid_name`, `invalid_email`, `plan_not_found`, `reason_required`, `reason_too_long`, `invalid_request`, `forbidden`.
  - Provider audit details: `reason`, `invitation {operation: first_admin.invite, id, email, role, expires_at}`, `after {slug, name, trust_domain, plan_id, plan_name}`.
  - The token is never returned, logged or audited. It goes only to the invitation email, sent after commit with the existing `invitation.Emailer`.
  - Separate package from S1's `internal/tenantlifecycle`, so the two don't collide.
- **Console:**
  - Tenants page: **Create tenant** button (super-admin) and `CreateTenantDialog` with name, slug (suggested from the name, live `ws-<slug>.zecurity.in` preview), plan ("Default plan (Free)" sends no `plan_id`), first admin's email and reason. The success notice links the tenant and shows the invitee and expiry.
  - `api/tenants.ts`, `lib/tenant-format.ts`.
  - `api-surface.test.ts`: `POST /provider/tenants` added on purpose.
- **Admin UI:** unchanged. `/invite/:token` signs in with Google; the sign-in takes the invitation; the callback's later accept call gets "already used", which it already ignores.
- **Known constraint (agreed 2026-11-18, not a bug):** the first admin's email must not already belong to another Zecurity workspace. A platform identity that already has a user resolves to its own workspace and never reaches the invitation (single-workspace login, ADR-024). The dialog says so.
- **Expired first-admin invitation:** sign-in is refused with `invitation_expired`, and the provider re-issues the invitation. See "Post-Phase Fixes" below (this replaced the first build's fall-through to self-signup).
- **Tests:**
  - `internal/bootstrap/tenant_create_test.go` (9):
    - equivalence with self-signup (status, trust domain, CA and keys row, Default policy, plan, `workspace.create`), and a chosen plan is honoured;
    - validation, with no partial writes;
    - **6 concurrent same-slug creates → exactly 1**;
    - email binding (a non-matching email doesn't join; an unverified matching email is refused) and single use;
    - **6 concurrent first sign-ins → exactly 1 admin**;
    - expiry;
    - only the provider invitation grants admin (DB CHECKs, tenant invites are `member`);
    - each single-use guard on its own (`export_test.go` `RunInvitedJoin`);
    - the accept endpoint refuses a pending admin invitation.
  - `internal/tenantcreate/handler_db_test.go` (3): relay-ops 403 with nothing written; 201 with audit actor, reason and invitation operation, and no token in the response, provider audit or tenant audit; the token only in the email; every error code.
  - `internal/client/invite_role_test.go`; `authz_test.go` pins `tenant.create` to super-admin.
  - Console: `Tenants.test.tsx` (create flow, default plan sends no `plan_id`, invalid slug, `slug_taken`), `tenant-format.test.ts`.
  - **Mutation checks, all caught:** verified-email check, expiry in the lookup, `pending` in the consume UPDATE, the membership row-count check, the accept-endpoint role guard, the client admin guard, the admin membership role, the token in the audit, and the authz check.
- **Gates (2026-11-18):** controller `go build ./...` and `go vet ./...` clean. Full `go test -count=1 ./...` with the DB and Valkey env: every package passes, 0 failures, only the 2 known skips. `graph/resolvers` and `internal/connector` were re-run with `-timeout 40m` because the host was under heavy load (load average ~10), and `certalert` needs `AUTH_TEST_VALKEY_URL` with a `/N` database. Provider console: 626 tests, lint and build clean.
- **Live check (2026-11-18, commit `35efd8b4`, reset DB, Orca embedded browser + lab):**
  - Console: Create tenant `acme-live-s2` (slug suggested, mixed-case email normalised, Free plan) → workspace active with trust domain, CA, Default policy, plan; no users; `admin`/`invited` membership; 7-day `admin` invitation. Token absent from the 201 body (`Cache-Control: no-store`), provider and tenant audit, controller log. The invitation email went out over the configured SMTP.
  - First-admin sign-in, through a local test IdP registered as the tenant's connection (the enterprise branch of `Provision`; the Google button on `/invite/:token` was not driven): non-matching email → `not_invited` (audited); same email without `email_verified` → refused, invitation still pending; matching verified email → Admin dashboard, invitation `accepted`, `invitation.accept` audited; a second account with the same email → refused; the used `/invite/:token` → "Invalid Invitation"; an expired invitation (second tenant, expiry backdated) → refused, nothing consumed.
  - API probes: `slug_taken` 409, 61-char slug, CRLF in email or name, reason only in the URL, no auth → refused; 6 parallel same-slug creates → 1×201 + 5×409, one workspace, invitation and audit row, no orphan CA key.
  - Lab, as the new tenant admin: relay on archlinux (.75), connector on luffy (.37) enrolled with the admin's token, shield on omarchy (.53) protecting `192.168.1.53:8000` (direct access from the dev box times out: `tcp dport 8000 drop`), group with the admin and the resource.
  - Client on the dev box, signed in as the first admin through the tenant's connection (`Logged in … Workspace: acme-live-s2`, SPIFFE `ws-acme-live-s2`, ACL 1 rule): tunnel up → `curl 192.168.1.53:8000` returns the page (connector: `access allowed`, `tunnel_opened ok` to the shield); tunnel down → times out; up again → works. **Live check PASS; no S2 defect found.**
  - Not live-tested: the client CLI admin-invitation guard (the shipped CLI never sends an invite token; unit-tested).
  - Lab notes: private-address relays need `ENV=development RELAY_DEV_ALLOW_PRIVATE_ADDR=true` on the controller; after a DB reset the client must be rebuilt with the new platform root (`ZECURITY_PLATFORM_ROOT_PEM_FILE`), since the root is compiled in.


### S2 Post-Phase Fixes

### Fix: Expired first-admin invitation locked the first admin out for good
**Issue:** found in the post-live review (2026-11-18). After the first-admin invitation expired, the invitee's first sign-in fell through to self-signup and created a workspace of their own. Their identity was then bound to that workspace (single-workspace login, ADR-024), so they could never take the tenant's admin seat. S2 had no way to issue a new invitation either, which left the tenant with no admin until someone edited the database.

**Root cause:** `bootstrap.Provision` treated "no usable invitation" the same as "never invited", and S2 had no re-issue path.

**Fix applied (decided by Sathiya, 2026-11-18):**
- `identity.ErrInvitationExpired` wraps `ErrNotInvited`, so audit and the client path treat it as "not invited". `Provision` returns it when the email still holds an unclaimed provider first-admin seat (an `invited` admin membership with an admin invitation) but no usable invitation (`hasExpiredFirstAdminSeat`; scoped to the workspace on an enterprise connection). No workspace is created.
- `auth/callback.go` redirects to `/login?error=invitation_expired`. The admin UI login page shows: "Your invitation to administer this workspace has expired. Ask your provider to send a new invitation, then use the link in that email." (no "Deploy one" link).
- **Re-issue:** `POST /provider/tenants/{id}/first-admin-invitation {admin_email?, reason}` → 201 `{tenant_id, invitation {id, email, expires_at}}`, with no token. It needs a reason. Code: `tenantcreate.Handler.Reinvite` → `bootstrap.ReissueFirstAdminInvitation`.
  - Action `tenant.first_admin.reinvite`, super-admin only (`decide()`).
  - Locks the workspace row. Marks every still-`pending` admin invitation `expired`, so earlier links stop working.
  - Then refuses if the tenant has an admin account (`first_admin_exists`). This check runs after the revoke on purpose: it sees a first sign-in that committed concurrently.
  - `admin_email` omitted re-sends to the previous invitee. Another address moves the seat; the old unclaimed seat row is deleted.
  - Issues a new 7-day invitation and refreshes the seat row (never one that belongs to an account: `email_has_account`).
  - Audits `invitation.create` (reissued) on the tenant stream and `tenant.first_admin.reinvite` (before/after, reason, never the token), then emails the link.
  - Errors: `not_found`, `tenant_not_active`, `first_admin_exists`, `email_has_account`, `invalid_email`, `reason_required`.
- **Tenant detail read:** `first_admin_invitation {status: pending|expired, expires_at}` while the seat is unclaimed, otherwise null. No email (OQ-2), no token. The statement-count pins moved from 6 to 7.
- **Console:** a tenant-detail "First admin" card (pending / expired) with **Send new invitation** (optional new email, reason). `api-surface.test.ts` adds the write.

**Tests:**
- `bootstrap`: an expired invitation → `ErrInvitationExpired` with nothing created; re-issue to the same email (the old link is revoked, the new one grants the seat once, refused after); re-issue revokes a still-pending link; re-issue to a new email moves the seat; refusals (admin exists, unknown, bad email, an account's email, inactive tenant).
- `tenantcreate`: HTTP re-issue (relay-ops 403, errors, no token in the body or audit, email sent, detail state pending → expired → pending → null).
- Admin `Login.test.tsx`; console `TenantDetailFirstAdmin.test.tsx` (5).
- Mutation checks, all caught: the expired refusal, revoking earlier links, dropping the old seat, the admin-exists check, the seat-upsert account guard, re-issue authz, the active-tenant check.

**Live (2026-11-18):** `expiry-co` with a backdated invitation → sign-in refused with the new message, nothing created. Console card "Invitation expired" → Send new invitation → old link "Invalid Invitation" (GET 404), new link valid, sign-in → Admin. Then the card is hidden and the API returns 409 `first_admin_exists`. Token absent from the audits and the log.

**Live re-run on `2426ed7e` (2026-11-18, also after a host restart), tenants `fix1-live` and `fix1-rerun`, Orca embedded browser:**
- Expired sign-in → `/login?error=invitation_expired` with the message; nothing created; invitation untouched.
- Console card "Invitation expired" → Send new invitation (reason) → card "Invitation pending"; old link GET 404, new link 200; reason and revoked id audited; token absent from both audits and the controller log.
- Sign-in with the new link → Admin, invitation `accepted`; card hidden; a further re-invite → 409 `first_admin_exists`.

PASS both runs.

**Related files:** `internal/identity/linker.go`, `internal/bootstrap/{bootstrap,tenant_create}.go`, `internal/auth/callback.go`, `internal/tenantcreate/handler.go`, `internal/provider/authz.go`, `internal/providerquery/tenants.go`, `cmd/server/main.go`, `admin/src/pages/Login.tsx`, `provider-console/src/{api/tenants.ts,api/types.ts,components/TenantFirstAdmin.tsx,pages/TenantDetail.tsx,lib/tenant-format.ts}`.

### Fix: Google sign-in could pick the wrong pending invitation
**Issue:** found in the same review. Platform (Google) login isn't scoped to a workspace. The invited-join lookup took `LIMIT 1` with no ordering, so an email with pending invitations in several workspaces joined an arbitrary one, and that first sign-in binds the identity to it for good (ADR-024). A first admin who also had a member invitation elsewhere could lose the admin seat.

**Fix applied (`bootstrap.Provision`, platform branch):** a deterministic order:
1. the workspace the person signed in for (`in.WorkspaceName`, which the invite link and the login page pass; matched on lower-cased name or slug);
2. a provider first-admin seat;
3. the oldest invitation (`invited_at`, then the workspace id).

The enterprise branch is already scoped to one workspace and is unchanged.

**Tests:** `TestPlatformInvitedJoin_ChoosesInvitationDeterministically`: with no hint, the seat wins over an older member invitation; with a hint naming the member workspace, that workspace wins; with two member invitations, the oldest wins. Mutation checks on each of the three keys are caught. Not live-checked: only Google reaches this branch (the local test login uses workspace connections).

**Security review note (automated, 2026-11-18, "ambiguous name matching"): acknowledged, no change.**
- The hint only orders the pending, unexpired invitations of the Google-verified email that is signing in, so it can't reach an invitation that person doesn't already hold.
- It is the signer's own choice, stored server-side with the sign-in state.
- Ranking the first-admin seat above the hint would send someone who deliberately opened another workspace's invite link to the seat instead.
- **Follow-up (design change, not S2):** carry the invitation id or token through `initiateAuth` so the invite link names the invitation exactly, instead of a name hint.

**Still open (not fixed, by choice):** the no-SMTP dev emailer prints the invite link with its token.

---

## Done when
B1, B2 and S2 are merged and live-checked; F8 and F5 are ticked in the tracker. F7 is ticked together with Sathiya's S1 and S4.
