---
type: phase
sprint: 26
owner: Barath (Person 1, M2)
features: [F34 billing plans and invoicing]
phases: [P1a, P1b, P2]
decisions: [D-62, D-63, D-64, D-65, D-66, D-67, D-68, D-43, D-46, D-58, D-59, D-02]
status: DRAFT pending sign-off (Q1 answered 2026-11-23: editable plan cards; Q2–Q6 use the recommended answers as working assumptions); no implementation until signed off
---

# Sprint 26 — Barath: P1 plan cards, P2 invoice generator (F34)

Read `Sprint26/path.md` first (order, conflict zones, rules). Binding decisions after sign-off: Decision Record amendment **billing**:
- **D-63** plan cards (create, and edit price and included quantities, any time);
- **D-64** money and rounding;
- **D-65** plan change in the month;
- **D-66** invoice lifecycle;
- **D-67** retention and purge (amends D-46);
- **D-68** who can read and write.

These build on **D-43** (plans), **D-58** (usage tenant snapshot), **D-59** (counts only) and **D-02** (single controller).

Order: **P1a (schema, merged first) → P1b (card API) and P2 (generator + lifecycle)**. P1a is the contract with Sathiya's P3 and P4: merge it early, on its own.

---

## P1a — Billing schema (`073_plan_cards.sql`, `074_invoices.sql`)

Check `ls controller/migrations | tail -3` first; latest expected is `072_usage_alert_kind.sql`.

### `073_plan_cards.sql`
A **plan card** = the B1 plan row (name, description, limits) + an optional **price**. Both parts are editable at any time (D-63).
- `plan_prices`, the price part of a card, **one row per plan** (absent = card not billable):
  - `plan_id UUID PRIMARY KEY REFERENCES plans(id) ON DELETE CASCADE` (deleting a plan follows B1's rules; invoices keep their own snapshot);
  - `currency TEXT NOT NULL CHECK (currency ~ '^[A-Z]{3}$')`;
  - `base_fee_minor BIGINT NOT NULL CHECK (base_fee_minor >= 0)`;
  - `meters JSONB NOT NULL CHECK (plan_price_meters_valid(meters))`;
  - `updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()`, `updated_by UUID NOT NULL` (provider user).
  - `plan_price_meters_valid(m JSONB)`:
    - an object whose keys are a subset of the D-57 meters (`relay_bytes_to_clients`, `relay_bytes_to_connectors`, `session_minutes`, `connector_minutes`, `shield_minutes`, `active_users`);
    - each value is exactly `{included, unit, unit_price_minor}`, all integers, `included >= 0`, `unit >= 1`, `unit_price_minor >= 0`;
    - an unknown key or a missing field is invalid. The empty object `{}` is valid (base fee only).
- `plans.revision INTEGER NOT NULL DEFAULT 1`: raised by every card edit (B1 fields or price), in the same transaction.
- `plan_card_revisions (plan_id UUID, revision INTEGER, card JSONB, changed_at TIMESTAMPTZ, changed_by UUID, reason TEXT, PRIMARY KEY (plan_id, revision))`: the **revision history**, one row per create or edit with the whole card (name, description, limits, price) as it was after the change. **Append-only** (UPDATE/DELETE/TRUNCATE refused, as `051_`); no foreign key, so a deleted plan's history stays readable.
- `usage_tenants`: `ALTER TABLE usage_tenants ADD COLUMN plan_id UUID` (no foreign key; it is a snapshot). It is kept current by the U1 collector and frozen at purge (D-65).
- Grants: `ztna_app` SELECT, INSERT, UPDATE, DELETE on `plan_prices`; SELECT, INSERT on `plan_card_revisions`.

### `074_invoices.sql`
- `invoices`:
  - Identity, tenant and status:
    - `id UUID PRIMARY KEY DEFAULT gen_random_uuid()`;
    - `number TEXT UNIQUE` (null while draft);
    - `tenant_id UUID NOT NULL` (**no foreign key**: invoices outlive a purged tenant, D-67);
    - `tenant_name TEXT NOT NULL`, `tenant_slug TEXT NOT NULL` (snapshot);
    - `month DATE NOT NULL CHECK (EXTRACT(DAY FROM month) = 1)`;
    - `status TEXT NOT NULL CHECK (status IN ('draft','issued','paid','void'))`.
  - Price snapshot and totals:
    - `currency TEXT NOT NULL`;
    - `plan_id UUID NOT NULL`, `plan_name TEXT NOT NULL`, `plan_revision INTEGER NOT NULL` (snapshot, no foreign key);
    - `subtotal_minor BIGINT NOT NULL CHECK (subtotal_minor >= 0)`.
  - Completeness: `collected_minutes INTEGER NOT NULL`, `month_minutes INTEGER NOT NULL`, `incomplete_hours INTEGER NOT NULL CHECK (incomplete_hours >= 0)`.
  - Lifecycle:
    - `generated_at TIMESTAMPTZ NOT NULL`;
    - `issued_at`, `issued_by`;
    - `paid_at DATE`, `paid_reference TEXT CHECK (char_length(paid_reference) <= 200)`, `paid_by`;
    - `voided_at`, `voided_by`, `void_reason TEXT`.
  - CHECKs tie the fields to the status: issued/paid/void have a `number` and `issued_at`; paid has `paid_at`; void has `void_reason`.
  - `CREATE UNIQUE INDEX invoices_one_per_month ON invoices (tenant_id, month) WHERE status <> 'void'`.
- `invoice_lines`:
  - `invoice_id UUID NOT NULL REFERENCES invoices(id) ON DELETE CASCADE`;
  - `position SMALLINT NOT NULL`, `kind TEXT NOT NULL CHECK (kind IN ('base','meter'))`, `meter TEXT` (null for base);
  - `quantity BIGINT`, `included BIGINT`, `unit BIGINT`, `billable_units BIGINT`, `unit_price_minor BIGINT`, `amount_minor BIGINT NOT NULL CHECK (amount_minor >= 0)`;
  - `PRIMARY KEY (invoice_id, position)`.
- `invoice_number_counters (year INTEGER PRIMARY KEY, last BIGINT NOT NULL)`: the gap-free counter (D-66), incremented with `SELECT … FOR UPDATE` (or `INSERT … ON CONFLICT DO UPDATE … RETURNING`) in the issue transaction.
- **Immutability triggers (D-66):**
  - `invoices` BEFORE UPDATE:
    - a draft may change freely;
    - `issued → paid` may set only the `paid_*` fields;
    - `issued → void` may set only the `void*` fields;
    - every other change of a non-draft row is refused (SQLSTATE 42501, as `051_`).
  - `invoices` BEFORE DELETE: only drafts.
  - `invoice_lines` BEFORE UPDATE: always refused. BEFORE INSERT/DELETE: only when the parent invoice is a draft. The CASCADE from a draft delete passes this check.
  - TRUNCATE refused on both tables.
- Grants: `ztna_app` SELECT, INSERT, UPDATE, DELETE on `invoices` and `invoice_lines`, and on `invoice_number_counters` (the triggers guard).

### Tests (DB-backed)
- Each CHECK refuses a bad row:
  - a meters object with an unknown key, a missing field or a negative value;
  - `effective_from` not on day 1;
  - a currency that isn't three capital letters;
  - a void invoice without a reason.
- `plan_prices`: a bad meters object refused; deleting a plan removes its price row. `plan_card_revisions`: UPDATE, DELETE and TRUNCATE refused; a duplicate (plan, revision) refused.
- `invoices`:
  - two non-void invoices for one tenant and month refused; void plus a new draft allowed;
  - an issued invoice: amount, lines, number and tenant name unchangeable; `issued → paid` and `issued → void` allowed; `paid → void` refused; deleting it refused;
  - lines of an issued invoice: insert, update and delete refused; TRUNCATE refused.

---

## P1b — Plan card API

### What exists
- B1 plans: the plan create and update routes, the `plan.*` actions, `provider.WithChange`, the console Plans page.

### Build
- `internal/billing/cards.go`:
  - `ValidatePrice(price)`: `currency == BILLING_CURRENCY`, meters match D-57, integers only, overflow-safe.
  - `SaveCard(tx, planID, b1Fields, price *Price, actor, reason)`: called by B1's create and update **in the same transaction**. It writes the plan fields (B1's existing code), upserts or deletes the `plan_prices` row (`price: null` = make the card unbillable), raises `plans.revision`, and inserts the `plan_card_revisions` row.
  - `CardFor(planID)`: the card as it is **now** (plan + price + revision), used by P2 at month close (D-65).
- Routes (B1's, extended; no new write route):
  - plan **create**: body gains an optional `price {base_fee_minor, meters}` → **create a card**.
  - plan **update**: body gains an optional `price` (omitted = unchanged, `null` = remove) → **edit a card**: name, description, limits, base fee, included quantities, units and unit prices, in any combination. `reason` required whenever the price or limits change.
  - plan **list / detail**: each plan returns its `price` (or null), `revision` and the number of tenants on it.
  - `GET /provider/plans/{id}/revisions`: the revision history (newest first, keyset-paged).
- Authorization: B1's plan actions (super-admin only, D-06); relay-ops 403.
- Audit: B1's plan create/update rows now carry the price in before/after, plus the revision number.
- Config: `BILLING_CURRENCY` (ISO 4217; startup fails on an invalid code when set). `BILLING_ENABLED=false` turns off the month-close job only; cards and reads keep working.

### Tests
- Create a card with and without a price.
- Edit only the price; edit only an included quantity; edit limits and price together → one revision, one audit row each.
- `price: null` removes the price (card unbillable); omitted leaves it.
- Currency mismatch, unknown meter, negative or non-integer values refused; nothing is written on a refused edit (revision unchanged).
- A failed edit (inject an error after the price write) leaves plan, price, revision and history unchanged.
- Relay-ops 403. The audit row has the reason and before/after with the price.
- **Mutation checks:**
  - revision not raised, or history row missing, on a price-only edit;
  - plan and price written in separate transactions;
  - currency not checked;
  - relay-ops allowed.

### Live check (P1)
Create two cards and a custom card for one tenant. Edit a card's price and an included quantity. The revision history shows each change, and the audit rows are present.

---

## P2 — Invoice generator and lifecycle

### What exists
- `providerquery.ReadMonthUsage(ctx, q, month)` (U2) gives every tenant's meters and active users for the month, plus the month's `CollectedMinutes` / `Minutes` (D-59 completeness). Purged tenants are named from `usage_tenants`.
- `usage_collector_minutes`: one row per collected minute. Incomplete hours are the hours of the month with fewer than 60 rows.
- U1 collector (`internal/usage`), `usage.FreezeTenant` in the S4 purge.
- A1 evaluator / U1 collector: patterns for a process-local ticker (D-02).

### Build
1. **Computation** (`internal/billing/compute.go`, pure, no DB):
   - `Compute(price, usage) → lines, subtotal`.
   - Base line: `amount = base_fee_minor`.
   - Per priced meter:
     - `over = max(0, quantity − included)`;
     - `billable_units = ceil(over / unit)` in integers: `(over + unit − 1) / unit`, overflow-checked;
     - `amount = billable_units × unit_price_minor`, overflow-checked.
   - `active_users` uses the month's distinct count.
   - Lines in a fixed order (base first, then meters in D-57 order). An overflow returns an error, and that tenant's invoice isn't generated (logged, retried next run).
2. **Month close** (`internal/billing/monthclose.go`):
   - A ticker runs hourly and at start (with jitter).
   - For the previous UTC month `M`, once `now >= start(M+1) + BILLING_SETTLE_DELAY` (default 6 h):
     - read `ReadMonthUsage(M)`;
     - for each tenant: plan = `workspaces.plan_id` for a live tenant, `usage_tenants.plan_id` for a purged one (D-65); card = `CardFor(plan)` as it reads **now** (at close); the invoice stores the card's revision;
     - if the card has no price, skip; if a non-void invoice already exists for (tenant, M), skip (an issued one is final, and an existing draft is regenerated only on request);
     - otherwise insert a **draft** with its lines in one transaction.
   - `ON CONFLICT` on the unique index makes a concurrent or repeated run a no-op.
   - Also handles **missed months**: when the controller was down at the close, the next start generates the drafts for every closed month that has no invoice yet, limited to the last 3 months. Older months are left to an explicit regenerate.
   - Tenants created after `M` get nothing for `M`. A tenant with zero usage still gets the base-fee invoice when priced.
3. **Lifecycle** (`internal/billing/lifecycle.go`). Each function runs in one transaction, returns before/after for the audit, and is the only implementation (P3's handlers call these):
   - `Regenerate(month, tenantID?, actor)`: recompute drafts for the month (or one tenant) with the **current** cards, replacing lines in place (same invoice id). This is how a card edit reaches an existing draft. Also creates a draft where none exists (e.g. after a void). Never touches issued, paid or void invoices.
   - `Issue(invoiceID, actor)`: draft → issued. Takes the next number for the issue year from `invoice_number_counters` in the same transaction: `INV-2026-000001`. A draft for the current, still-open month can't be issued.
   - `MarkPaid(invoiceID, paidAt, reference, actor)`: issued → paid.
   - `Void(invoiceID, reason, actor)`: issued → void (a draft is deleted rather than voided).
4. **U1 snapshot:** the collector's `usage_tenants` upsert also writes `plan_id`; `FreezeTenant` freezes it. The purge itself doesn't change (invoices have no foreign key).
5. Wire in `cmd/server/main.go` (own block). Config:
   - `BILLING_ENABLED` (default true when `BILLING_CURRENCY` is set);
   - `BILLING_SETTLE_DELAY` (default `6h`; tests and the lab use a short value);
   - an injectable clock for tests.

### Tests (DB-backed, with mutation checks)
- `Compute` (table tests):
  - included quantity not reached → 0;
  - exactly reached → 0;
  - one byte over → 1 unit;
  - large values near `int64` max → error, not a wrap;
  - empty meters → base fee only;
  - included quantity raised above the usage → that meter bills 0.
- Month close:
  - UTC boundaries;
  - the settling delay respected;
  - two runs → one invoice;
  - an unpriced plan → none;
  - a card edited before the close → the draft uses the edited card; edited after the draft → unchanged until regenerate; edited after issue → the issued invoice unchanged;
  - a missed month generated on start;
  - a tenant created after the month → none.
- **Plan change mid-month (D-65):** billed the whole month at the plan held at close.
- Incomplete hours: 5 missing minutes in one hour → `incomplete_hours = 1`, quantities as counted.
- Purged tenant: final invoice named from `usage_tenants`, plan from the frozen `plan_id`; its earlier invoices survive the purge.
- Lifecycle:
  - regenerate replaces lines and keeps the id;
  - issue numbers in order, and 20 concurrent issues give 20 distinct, gap-free numbers;
  - issuing an open month refused;
  - paid and void transitions; void without a reason refused;
  - after a void, regenerate creates a new draft.
- **Mutation checks:**
  - floor instead of ceil;
  - float arithmetic;
  - quantities read outside `ReadMonthUsage`;
  - the unique index removed (double invoice);
  - numbering from a `SEQUENCE` (gap on rollback);
  - the trigger allowing an issued invoice to change;
  - plan taken at month start instead of close;
  - the purge deleting invoices;
  - incomplete hours not recorded.

### Live check (P2)
- Two tenants with known usage in a closed month (short settling delay, or the test clock): each draft's lines match U3's report and the cards, checked by hand. Edit a card and regenerate: the draft follows; an issued invoice doesn't.
- A 5-minute controller stop shows as incomplete hours.
- Regenerate twice → one invoice. Issue → number, and a SQL `UPDATE` is refused. Mark paid; void with a reason.
- Purge a tenant → its final invoice exists and its invoices survive.

### Implementation record
_Fill in when built (branch, choices, tests, gates, mutation checks)._

---

## Post-Phase Fixes

_None yet._
