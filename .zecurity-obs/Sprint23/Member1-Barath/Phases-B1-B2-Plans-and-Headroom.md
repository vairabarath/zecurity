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
  - **`workspaces.plan_id`:** FK, `NOT NULL`, **no column default**. A column default is a fixed value and can't follow a changed default plan.
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
  - **Every workspace insert sets `plan_id` explicitly** to the **current** default (`SELECT id FROM plans WHERE is_default`), in the same transaction:
    - `internal/bootstrap/bootstrap.go` (self-signup);
    - S2's provider create;
    - test helpers that insert workspaces.
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

---

## Done when
B1, B2 and S2 are merged and live-checked; F8 and F5 are ticked in the tracker. F7 is ticked together with Sathiya's S1 and S4.
