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
    - `headroom {used_bytes_per_sec, headroom_bytes_per_sec, utilization, over_capacity}`, where **used = rate to clients + rate to connectors** (both flows leave through the same link: a relay forwards every byte it receives), **headroom = capacity − used**, clamped at 0, `utilization = used / capacity`, and `over_capacity` is true when used exceeds capacity. The capacity is the link's usable throughput in its **tighter direction** (D-42, refined 2026-11-18);
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
- **Headroom maths:** the sum of both flows, the clamp, `utilization`, `over_capacity`, unset capacity, one or both rates still measuring, and revoked/not reported → null.
- **Authz:** relay-ops and super-admin allowed, others refused.
- **Audit:** before/after rows.
- **Console:** formatting and units, the "Not configured" label, the estimate wording, and the attention threshold.

### Live check
- Set the capacity on a lab relay (as relay-ops).
- Push known traffic (as in the F5 run: `curl` a large file through the relay): the two measured flows, the capacity and the headroom (capacity − both flows) appear separately on the list, the detail and the Overview.
- A rate above capacity shows headroom 0 and "over capacity".
- Clear the capacity: it shows "Not configured".
- The capacity change is audited.


### B2 implementation record (branch `feat/sprint23-b2-headroom`)
- **D-42 refined (2026-11-18, approved by Barath):** **one capacity per relay**, the link's usable throughput in its **tighter direction**; **headroom = capacity − (to clients + to connectors)**. A relay forwards every byte it receives, so both flows share its link; a per-direction headroom would understate how full it is (e.g. 60 + 50 MB/s on a 125 MB/s link reads 65 and 75 MB/s free per direction, but only 15 MB/s is really left). Updated in the Decision Record, `path.md`, this file and `CLAUDE.md`.
- **Schema `063_relay_link_capacity.sql`:** `relays.link_capacity_bps BIGINT NULL CHECK (> 0)`, in bytes per second like F5's bandwidth.
- **Controller:**
  - `internal/relay/capacity.go`:
    - `Store.SetLinkCapacity`: row lock; revoked/deleted → `already_revoked` / `already_deleted`; the same value is a no-op (not audited); audit in the same transaction;
    - `PUT /provider/relays/{id}/link-capacity` `{bps | null, reason}`: `null` clears; a missing `bps` → `invalid_request`; ≤ 0, fractional, a string, or above **400 Gbit/s** (`MaxLinkCapacityBps` = 5×10¹⁰ B/s, which catches bit/byte unit slips) → `invalid_capacity`; the reason is required;
    - audited as `relay.capacity.set` with before/after.
  - Action `relay.capacity.set` (under `relay.*`, so relay-ops and super-admin) and `Authz.CanSetRelayCapacity`.
  - `providerquery/relays.go`: relay reads gain `link_capacity_bps` and a derived **`headroom {used_bytes_per_sec, headroom_bytes_per_sec, utilization, over_capacity}`**. It's computed on read, never stored, and `null` unless the capacity and both rates are known.
- **Console:**
  - `lib/capacity.ts`: Mbps/Gbps ⇄ bytes/s (1 Gbps = 125,000,000 B/s), link-speed formatting, `NEAR_CAPACITY = 0.9`.
  - `components/Headroom.tsx`:
    - `HeadroomCell`: "Not configured", "N free of 1 Gbps", a warning at ≥ 90 %, "Over capacity";
    - `HeadroomDetail`: link capacity, use of both directions, headroom and utilization, shown separately, with the estimate note;
    - `RelayCapacityCard` with `SetLinkCapacityDialog`: "enter the slower direction", Mbps/Gbps, reason, and Clear capacity; hidden for revoked and deleted relays.
  - A Relays list Headroom column, a relay detail **Link capacity** card, and an Overview attention item ("Near capacity" at ≥ 90 %, "Over capacity" above 100 %).
  - `api/relays.ts` `setRelayLinkCapacity`; `api-surface.test.ts` adds the one write.
  - The relay detail button pins include "Set link capacity" for both roles.
- **Tests:**
  - Controller:
    - `TestRelayHeadroom`: the summed flows (60 + 50 of 125 MB/s → 15 free, 88 %), the clamp and `over_capacity`, exactly full, idle, and every unknown → nil;
    - `TestLinkCapacity_*`: relay-ops sets, audit before/after, an unchanged value not audited, `null` clears, validation, terminal and unknown relays;
    - `TestRelayReads_LinkCapacityAndHeadroom`: list and detail;
    - the DTO pin for `link_capacity_bps`, `headroom` and `RelayHeadroom`;
    - **mutation-checked**: counting one direction only fails both headroom tests.
  - Console: `capacity.test.ts` (5), `Headroom.test.tsx` (9), Overview near/over capacity.
- **Gates (2026-11-18):** controller `go vet ./...` clean; full `go test ./...` **1,911 passed, 0 failed** (2 known skips); provider console **636** tests, lint and build clean.
- **Live check:** pending (reset DB; set a capacity on a lab relay; push known traffic; headroom = capacity − both flows).

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

---

## Done when
B1, B2 and S2 are merged and live-checked; F8 and F5 are ticked in the tracker. F7 is ticked together with Sathiya's S1 and S4.
