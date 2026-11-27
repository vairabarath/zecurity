---
type: phase
sprint: 26
owner: Barath (Person 1, M2)
features: [F34 billing plans and invoicing]
phases: [P1a, P1b, P2]
decisions: [D-62, D-63, D-64, D-65, D-66, D-67, D-69, D-70, D-43, D-46, D-58, D-59, D-02]
status: signed off 2026-11-23 (by Sathiya, overriding the product-owner sign-off step; Barath reviews in PR #249); ready once the Sprint 25 stack is merged
---

# Sprint 26 — Barath: P1 plan cards and billing settings, P2 invoice generator (F34)

Read `Sprint26/path.md` first (order, conflict zones, rules). Binding decisions: Decision Record amendment **2026-11-23, billing**:
- **D-63** plan cards (create and edit any time);
- **D-64** country and currency (editable, default US / USD; a change switches every card);
- **D-65** month-start plan (a mid-month change bills from next month);
- **D-66** invoice lifecycle;
- **D-67** retention (editable, default 8 years) and purge (amends D-46);
- **D-69** tax (default none, editable rate and label);
- **D-70** who can read and write.

These build on **D-43** (plans), **D-58** (usage tenant snapshot), **D-59** (counts only) and **D-02** (single controller).

Order: **P1a (schema, merged first) → P1b (cards + settings API) and P2 (generator + lifecycle)**. P1a is the contract with Sathiya's P3 and P4: merge it early, on its own.

---

## P1a — Billing schema (`073_plan_cards.sql`, `074_invoices.sql`)

Check `ls controller/migrations | tail -3` first; latest expected is `072_usage_alert_kind.sql`.

### `073_plan_cards.sql`
- **`billing_settings`**, one row (`id BOOLEAN PRIMARY KEY DEFAULT true CHECK (id)`):
  - `country TEXT NOT NULL DEFAULT 'US' CHECK (country ~ '^[A-Z]{2}$')` (ISO 3166-1 alpha-2);
  - `currency TEXT NOT NULL DEFAULT 'USD' CHECK (currency ~ '^[A-Z]{3}$')` (ISO 4217);
  - `tax_rate_bp INTEGER NOT NULL DEFAULT 0 CHECK (tax_rate_bp BETWEEN 0 AND 10000)`, `tax_label TEXT NOT NULL DEFAULT '' CHECK (char_length(tax_label) <= 32)`;
  - `invoice_retention_years SMALLINT NOT NULL DEFAULT 8 CHECK (invoice_retention_years BETWEEN 1 AND 30)`;
  - `email_tenants BOOLEAN NOT NULL DEFAULT true`;
  - `updated_at`, `updated_by`.
  - Seeded with the defaults.
- **`plan_prices`**, the price part of a card, **one row per plan** (absent = card not billable):
  - `plan_id UUID PRIMARY KEY REFERENCES plans(id) ON DELETE CASCADE` (deleting a plan follows B1's rules; invoices keep their own snapshot);
  - `currency TEXT NOT NULL CHECK (currency ~ '^[A-Z]{3}$')`;
  - `base_fee_minor BIGINT NOT NULL CHECK (base_fee_minor >= 0)`;
  - `meters JSONB NOT NULL CHECK (plan_price_meters_valid(meters))`;
  - `updated_at`, `updated_by`.
  - `plan_price_meters_valid(m)`:
    - keys are a subset of the D-57 meters;
    - each value is exactly `{included, unit, unit_price_minor}`, all integers, `included >= 0`, `unit >= 1`, `unit_price_minor >= 0`;
    - `{}` is valid (base fee only).
- `plans.revision INTEGER NOT NULL DEFAULT 1`: raised by every card edit and by a currency change.
- **`plan_card_revisions (plan_id, revision, card JSONB, changed_at, changed_by, reason, PRIMARY KEY (plan_id, revision))`**: append-only history of the whole card after each change. UPDATE, DELETE and TRUNCATE are refused, as in `051_`. No foreign key, so a deleted plan's history stays.
- **`tenant_billing_plans (tenant_id UUID, month DATE CHECK (day = 1), plan_id UUID, recorded_at, PRIMARY KEY (tenant_id, month))`**: the **month-start plan** (D-65), written by the first plan change in a month. No foreign keys (it is a snapshot; it outlives a purge until the invoice exists). Append-only (UPDATE refused).
- `usage_tenants`: `ADD COLUMN plan_id UUID` (snapshot; kept current by U1, frozen at purge).
- Grants for `ztna_app`:
  - SELECT, UPDATE on `billing_settings`;
  - SELECT, INSERT, UPDATE, DELETE on `plan_prices`;
  - SELECT, INSERT on `plan_card_revisions` and `tenant_billing_plans`.

### `074_invoices.sql`
- **`invoices`**:
  - Identity, tenant and status:
    - `id`, `number TEXT UNIQUE` (null while draft);
    - `tenant_id UUID NOT NULL` (**no foreign key**, D-67), `tenant_name`, `tenant_slug` (snapshot);
    - `month DATE CHECK (day = 1)`;
    - `status CHECK (status IN ('draft','issued','paid','void'))`.
  - Price and tax snapshot:
    - `currency`, `plan_id`, `plan_name`, `plan_revision`;
    - `subtotal_minor`, `tax_rate_bp`, `tax_label`, `tax_minor`, `total_minor` (all `>= 0`; CHECK `total_minor = subtotal_minor + tax_minor`).
  - Completeness: `collected_minutes`, `month_minutes`, `incomplete_hours`.
  - Lifecycle:
    - `generated_at`, `issued_at`, `issued_by`;
    - `paid_at DATE`, `paid_reference (<= 200 chars)`, `paid_by`;
    - `voided_at`, `voided_by`, `void_reason`.
  - CHECKs tie the fields to the status.
  - `CREATE UNIQUE INDEX invoices_one_per_month ON invoices (tenant_id, month) WHERE status <> 'void'`.
- **`invoice_lines`**:
  - `invoice_id REFERENCES invoices ON DELETE CASCADE`, `position`, `kind (base|meter)`, `meter`;
  - `quantity`, `included`, `unit`, `billable_units`, `unit_price_minor`, `amount_minor`;
  - `PRIMARY KEY (invoice_id, position)`.
- **`invoice_number_counters (year INTEGER PRIMARY KEY, last BIGINT NOT NULL)`**: the gap-free counter.
- **`invoice_emails`**, the delivery queue for D-68:
  - `invoice_id`, `kind (issued|voided|resend)`, `seq` (1 for issued/voided, n for the n-th resend);
  - `status (pending|sending|sent|failed|not_delivered)` (`sending` = claimed by the worker; a row left in `sending` by a restart becomes `failed`, never re-sent automatically), `recipients_count`, `attempts`, `next_attempt_at`, `last_error` (sanitized, no addresses), `sent_at`, `created_at`;
  - an index on `(status, next_attempt_at)` for the worker's claim;
  - `PRIMARY KEY (invoice_id, kind, seq)`.
  - Sathiya's P3 owns the sender; the table is here so the contract lands with P1a.
- **Triggers (D-66):**
  - `invoices` BEFORE UPDATE:
    - a draft may change;
    - `issued → paid` sets only `paid_*`;
    - `issued → void` sets only `void*`;
    - every other change refused (42501).
  - `invoices` BEFORE DELETE: drafts only, **or** inside `prune_invoices()`.
  - `invoice_lines` BEFORE UPDATE: refused. INSERT/DELETE: only when the parent is a draft (or in the prune). TRUNCATE refused on both.
- **`prune_invoices(cutoff DATE)`**: `SECURITY DEFINER`. It sets a transaction-local flag the delete triggers accept, deletes invoices (and their lines and emails) with `month < cutoff`, and returns the count. It is the only path that deletes issued invoices (D-67).
- Grants for `ztna_app`:
  - SELECT, INSERT, UPDATE, DELETE on `invoices`, `invoice_lines`, `invoice_number_counters` and `invoice_emails` (the triggers guard);
  - EXECUTE on `prune_invoices`.

### Tests (DB-backed)
- Each CHECK refuses a bad row:
  - an invalid country or currency;
  - a tax rate over 10000;
  - retention outside 1–30;
  - an invalid meters object;
  - `total ≠ subtotal + tax`;
  - a void without a reason.
- `plan_card_revisions` and `tenant_billing_plans` refuse UPDATE; a duplicate (tenant, month) is refused.
- `invoices`:
  - two non-void invoices for one tenant and month refused;
  - an issued invoice unchangeable, except `issued → paid` / `issued → void`; `paid → void` refused;
  - deleting an issued invoice refused outside `prune_invoices`, allowed inside it;
  - lines of an issued invoice unchangeable; TRUNCATE refused.

---

### Implementation record (P1a)
- **Built by Sathiya on 2026-11-26** (Sathiya took over P1a, P1b and P2 on Sathiya's instruction, because P1a blocked every other phase; Barath reviews). Branch `feat/sprint26-p1a-billing-schema`, commit `349ea314`, local only.
- **As specified,** plus:
  - `invoice_lines` re-checks the arithmetic: a meter line's `billable_units` must be the rounded-up units above the included quantity, and `amount = units × unit price` (a bigint overflow raises). A base line has no meter fields.
  - `invoice_emails` CHECKs: `seq = 1` unless `resend`; `sent_at` is set exactly when `sent`.
  - `prune_invoices(cutoff)` also **refuses a cutoff inside the retention** of `billing_settings`, so a wrong caller can't shorten it. The delete trigger accepts the prune only when its flag is set **and** the statement runs as the function's owner: the app role can set the flag itself but can't become the owner.
  - **Security fix before commit:** every function lists `pg_temp` last in its `search_path` and qualifies its tables. Otherwise `pg_temp` is searched first, and a temporary `invoices` or `billing_settings` table could fool a guard. Found by the automated security review.
  - The seeded plans get revision-1 history rows.
- **Tests** (`internal/billing/schema_db_test.go`):
  - defaults, every CHECK, append-only tables, the lifecycle transitions, the email queue and grants;
  - prune, including the app role faking the flag;
  - temp-table shadowing.
- **Mutation checks**, each caught: no unique index; paid changing other fields; the flag without the owner check; the prune ignoring retention; round-down units; lines of an issued invoice changeable; shadowing (qualification and `pg_temp` removed).
- **Gate:** `go build`, `go vet`, the full `go test ./...` with the DB and Valkey env: 50 packages ok, only the 2 known hard-coded skips.

## P1b — Plan card and billing settings API

### What exists
- B1 plans: the create, update and assign routes, the `plan.*` / `tenant.plan.assign` actions, `provider.WithChange`, the console Plans page.

### Build
- **`internal/billing/settings.go`**:
  - `GetSettings`.
  - `UpdateSettings(patch, actor, reason)`, in one transaction:
    - **country**: set it and fill in its default currency from a small built-in ISO table (e.g. `US→USD`, `IN→INR`, `GB→GBP`, `DE→EUR`; an unknown country requires an explicit currency);
    - **currency**: validated against ISO 4217 with its minor-unit exponent;
    - when the currency changes, **every `plan_prices` row** is updated to it, each card's revision is raised, and a revision row with that reason is written (D-64). Amounts are not converted;
    - tax rate and label, retention years, `email_tenants`.
  - Audit `billing.settings.update` with before/after; a currency change also lists the cards it switched.
- **`internal/billing/cards.go`**:
  - `ValidatePrice` (currency must equal the settings currency; meters; integers; overflow).
  - `SaveCard(tx, planID, b1Fields, price, actor, reason)`, called by B1's create and update **in the same transaction**: plan fields, upsert or delete the price (`null` = unbillable), raise the revision, write the revision row.
  - `CardFor(planID)` (the card now).
- **`internal/billing/planmonth.go`** (D-65):
  - `RecordMonthStartPlan(tx, tenantID, oldPlanID, now)`: inserts `(tenant, month(now), oldPlan)` with `ON CONFLICT DO NOTHING`. It is called in **B1's assign transaction before** the plan changes, so the first change in a month keeps the month-start plan and later changes in the same month keep it too.
  - `PlanForMonth(tenantID, month)`: the `tenant_billing_plans` row if there is one; else the live `workspaces.plan_id`; else (purged) `usage_tenants.plan_id`.
- **U1 snapshot:** the collector's `usage_tenants` upsert also writes `plan_id`; `FreezeTenant` freezes it.
- **Routes:**
  - `GET /provider/billing/settings`;
  - `PATCH /provider/billing/settings` (reason required), action **`billing.settings.write`**;
  - plan create and update gain an optional `price` (omitted = unchanged, `null` = remove; a reason is required when the price or limits change);
  - plan list and detail return `price`, `revision` and the number of tenants;
  - `GET /provider/plans/{id}/revisions`.
- Super-admin only; relay-ops 403. Config: `BILLING_ENABLED` (month-close job on/off; default on).

### Tests
- **Settings:**
  - the defaults read US / USD, 0 tax, 8 years, email on;
  - setting `IN` fills in `INR`;
  - a currency change switches every card and raises each revision (a mutation check removes one card from the update);
  - a failed currency change (inject an error) leaves settings and all cards unchanged;
  - invalid values refused.
- **Cards:**
  - create with and without a price; edit price only, included quantity only, limits and price together → one revision and one audit row each;
  - `price: null` and omitted;
  - a price in a currency other than the settings currency refused;
  - a failed edit changes nothing.
- **Month-start plan:**
  - assign twice in one month → the row keeps the plan from the start of the month;
  - a change in the next month records that month's start plan;
  - a tenant created mid-month and never changed → `PlanForMonth` = creation plan;
  - a card edited mid-month (e.g. Starter ₹499 / 50 GB / ₹2 per GB → ₹599 / 100 GB / ₹1 on the 15th) → that month's draft uses ₹499 / 50 GB / ₹2, the next month's uses the new terms; a currency change mid-month applies from the next month too; a card created mid-month → its first revision;
  - a purged tenant → the frozen plan.
- Relay-ops 403; audit rows carry the reason and before/after.
- **Mutation checks:**
  - currency change missing a card;
  - `RecordMonthStartPlan` after the change (records the new plan);
  - `ON CONFLICT DO UPDATE` (the last change overwrites the month start);
  - revision not raised;
  - relay-ops allowed.

### Live check (P1)
- Settings default, then switch to India / INR: every card switched. Tax 18 % GST.
- Create two cards and a custom card, then edit a card; the history shows each change.
- Move a tenant to another card mid-month: the `tenant_billing_plans` row holds the month-start card, and the limits changed immediately.

### Implementation record (P1b)
- **Built by Sathiya on 2026-11-26** (takeover, as P1a). Branch `feat/sprint26-p1b-cards-settings` on P1a, commit `a7625489`, local only.
- **`internal/billing`:**
  - `settings.go`: `GetSettings`, and `UpdateSettings` in one transaction; a currency change switches every priced card (plan rows locked in id order).
  - `cards.go`: `DecodePrice` (strict integers, 18-digit cap → `price_overflow`), `ValidatePrice`, `CreateCard` / `SaveCard` (one edit = one revision), `CardFor`, `CardForMonth`.
  - `planmonth.go`: `RecordMonthStartPlan`, `PlanForMonth`, `PlanName`.
  - `currency.go`: ISO 4217 codes and a 54-country table, served as `countries`.
- **B1:**
  - create and update take `price` (absent = unchanged, `null` = remove). It is checked against the settings currency, read `FOR SHARE`, so a card write and a currency switch serialise.
  - create takes an optional `reason` (stored as "Card created" when empty).
  - assign records the month-start plan before the change.
- **API:**
  - `GET` / `PATCH /provider/billing/settings`;
  - `GET /provider/plans/{id}/revisions`;
  - plan reads return `price` and `revision`;
  - tenant detail returns `billing_plan`;
  - the U1 snapshot keeps `plan_id`.
- **Choices to review:**
  1. **`PlanForMonth` has one extra step:** the month's row, else the **earliest later month's row**, else the live plan, else the frozen plan. With no change in between, the start plan of that later month is the plan the tenant kept. Without this step, regenerating October after a November change would bill October at the November plan, which D-65 forbids. Test `TestMonthStartPlan` covers it; the mutation back to the plain rule is caught.
  2. **Reading** the settings uses `plan.read` (the Plans page needs the currency). D-70 names only the write action. It is super-admin only either way.
- **Tests:** `internal/plans/cards_db_test.go` (cards, settings, currency switch, atomic failure, month-start plan, card for month), plus the snapshot `plan_id` in `internal/usage`.
- **Mutation checks**, each caught: the switch missing a card; recording after the change; `ON CONFLICT DO UPDATE`; revision not raised; relay-ops allowed; no later-change rule; the card as it reads now; the currency check skipped; the snapshot `plan_id` not frozen.
- **Gate:** the full suite passed, 50 packages ok, 2 known skips.

---

## P2 — Invoice generator and lifecycle

### What exists
- `providerquery.ReadMonthUsage` (U2): every tenant's meters and active users for the month, plus `CollectedMinutes` / `Minutes`. `usage_collector_minutes` gives the incomplete hours.
- P1b: `PlanForMonth`, `CardFor`, `GetSettings`.

### Build
1. **Computation** (`internal/billing/compute.go`, pure):
   - `Compute(card, usage, settings) → lines, subtotal, tax, total`.
   - Base line = base fee.
   - Per priced meter (in D-57 order):
     - `over = max(0, quantity − included)`;
     - `billable_units = (over + unit − 1) / unit`;
     - `amount = billable_units × unit_price_minor`, overflow-checked.
   - `tax = (subtotal × tax_rate_bp + 5000) / 10000` (half up, overflow-checked); `total = subtotal + tax`.
   - An overflow → an error; that tenant is skipped, logged and retried on the next run.
2. **Month close** (`internal/billing/monthclose.go`):
   - An hourly ticker, also run at start.
   - For each closed UTC month `M` within the last 3 months with `now >= start(M+1) + BILLING_SETTLE_DELAY` (default 6 h):
     - read `ReadMonthUsage(M)` and the settings;
     - for each tenant:
       - plan = `PlanForMonth(tenant, M)` (**month-start plan**, D-65);
       - card = `CardForMonth(plan, M)`: the revision **in force at the start of M** (D-63, changed 2026-11-24) — the latest `plan_card_revisions` row with `changed_at < M`, else the card's first revision (a card created during M) — with that revision's price, currency and revision number. Never the card as it reads now;
       - no price → skip; a non-void invoice already exists → skip;
       - otherwise insert a **draft** with its lines in one transaction (`ON CONFLICT` on the unique index = no-op).
   - Tenants created after `M` get nothing. A priced tenant with zero usage gets the base fee.
3. **Lifecycle** (`internal/billing/lifecycle.go`; the only implementation; each returns before/after):
   - `Regenerate(month, tenantID?, actor)`: recompute drafts with the current cards and settings (same id), and create missing drafts (e.g. after a void). Never touches issued, paid or void invoices.
   - `Issue(invoiceID, actor)`: draft → issued with the next `INV-<year>-<000001>` from the counter, in the same transaction. Issuing the current, open month is refused. It **enqueues** an `invoice_emails (issued, 1)` row in the same transaction when `email_tenants` is on (P3 sends it).
   - `MarkPaid(invoiceID, paidAt, reference, actor)`: issued → paid.
   - `Void(invoiceID, reason, actor)`: issued → void, and enqueues `(voided, 1)` when email is on. A draft is deleted, not voided.
4. **Retention** (D-67): once a day, call `prune_invoices(first day of (current month − retention years))`. Audit `invoice.prune` with the count.
5. Wire in `cmd/server/main.go` (own block). Config: `BILLING_SETTLE_DELAY` (default `6h`; short on the lab) and an injectable clock for tests.

### Tests (DB-backed, with mutation checks)
- **`Compute`:**
  - included quantity not reached → 0; exactly reached → 0; one over → 1 unit;
  - tax at 0, at 1800 bp, and at a half-unit boundary (rounds up);
  - near `int64` max → error;
  - empty meters → base fee only.
- **Month close:**
  - UTC boundaries; the settling delay;
  - two runs → one invoice;
  - an unpriced card → none;
  - a missed month generated on start (3-month limit);
  - a tenant created after the month → none.
- **Month-start plan:** a mid-month change → billed at the month-start card; the next month at the new card.
- **Card, currency and tax changes:** before the close → used; after the draft → used on regenerate; after issue → the issued invoice unchanged.
- Incomplete hours stated. A purged tenant's final invoice is named from the snapshot, and its invoices survive the purge.
- **Lifecycle:**
  - 20 concurrent issues → 20 gap-free numbers;
  - issuing the open month refused;
  - an email row enqueued once on issue and void (none when the setting is off);
  - after a void, regenerate creates a new draft.
- **Retention:** prunes only invoices older than the setting; a changed setting moves the cutoff.
- **Mutation checks:**
  - floor instead of ceil;
  - float arithmetic;
  - tax truncated;
  - quantities read outside `ReadMonthUsage`;
  - plan taken as the current plan instead of `PlanForMonth`;
  - card taken as it reads now instead of the month-start revision (`CardForMonth`);
  - no unique index;
  - `SEQUENCE` numbering;
  - the trigger allowing an issued invoice to change;
  - the purge deleting invoices;
  - the prune ignoring the setting;
  - an email enqueued twice.

### Live check (P2)
- Two tenants with known usage in a closed month: each draft's lines, tax and total match U3's report, the cards and the settings, checked by hand. A 5-minute controller stop shows as incomplete hours.
- The mid-month plan change bills the month-start card.
- Regenerate twice → one invoice. Issue → number, and a SQL `UPDATE` is refused. Mark paid; void with a reason.
- Purge a tenant → its final invoice exists and its invoices survive.

### Implementation record
- **Built by Sathiya on 2026-11-26** (takeover). Branch `feat/sprint26-p2-invoices` on P1b, commit `261219e7`, local only.
- **Files:**
  - `compute.go`: `Compute`, `BillableUnits` and `Tax`, overflow-checked;
  - `monthclose.go`: `Generator` (`CloseDueMonths`, `Regenerate`, `DueMonths`);
  - `lifecycle.go`: `Issue`, `MarkPaid`, `Void`, `Resend` (here so there is one implementation; P3 calls it), `Prune`;
  - `invoice.go`: rows, draft insert and rewrite.
- `providerquery.MonthUsage.IncompleteHours` is new (`json:"-"`: the usage API output is unchanged).
- `main.go`: the month close, a daily audited prune (`invoice.prune`), and `BILLING_ENABLED` / `BILLING_SETTLE_DELAY`.
- **Choices to review:**
  1. **Currency after a mid-month switch.** A draft takes the currency of the card revision in force on the 1st (D-63, changed 2026-11-24). So a past month regenerated after a currency switch keeps the old currency and amounts. D-64's line "drafts take the new currency when regenerated" reads the other way; I followed the later D-63 change and P1b's test line "a currency change mid-month applies from the next month too".
  2. A tenant purged during the month gets a final invoice even with no usage (from `usage_tenants.purged_at`). A tenant soft-deleted and not yet purged, with no usage, gets none.
  3. **Regenerate** refuses the open month (`month_open`). **Void** refuses drafts and paid invoices (`invoice_not_issued`); there is no draft-delete route.
  4. **Resend** queues an email even when `email_tenants` is off (the operator asked for it).
  5. **Paid date:** not in the future and not before the invoice's month.
- **Tests:** `internal/billing/invoices_db_test.go`:
  - `Compute`;
  - the month close (amounts, a 5-minute stop shown as 1 incomplete hour, settle delay, unpriced card, late tenant, base fee only, two runs → one invoice), the catch-up window, missed months;
  - the month-start plan and card; reads only through U2;
  - regenerate after tax, card and currency changes; the lifecycle and its refusals; email off;
  - 20 concurrent issues get gap-free numbers, and a rolled-back issue leaves no gap;
  - a purged tenant's final invoice; prune;
  - plus "invoices survive the purge" in `tenantlifecycle` and a no-floating-point guard.
- **Mutation checks**, each caught: floor; tax truncated; no overflow check; the current plan; the card as it reads now; own SQL for quantities; an unpriced card invoiced; regenerate on an issued invoice; email ignoring the setting; the open month issuable; the prune ignoring the setting; incomplete hours dropped; float rounding.
- **Gate:** the full suite passed, 50 packages ok, 2 known skips.

---

## Post-Phase Fixes

### Fix: purged tenant's snapshot pruned before its final invoice (acceptance 2026-11-26)
**Issue:** tenant `s26-charlie` (Card C on Nov 1, moved to a custom card, then suspended, deleted and purged on Nov 26, no usage) got no November invoice at the month close.

**Root cause:** `usage.Collector.Prune` (U1, `controller/internal/usage/collector.go`) deleted every purged `usage_tenants` snapshot without usage rows. P2's `billedTenants` finds a tenant purged during the month through that snapshot (`purged_at` in the month), and `PlanForMonth` falls back to its frozen `plan_id`; both ran after the month closed, when the row was already gone.

**Fix applied:**
```go
// BEFORE:
DELETE FROM usage_tenants t WHERE t.purged_at IS NOT NULL AND NOT EXISTS (usage_hourly …) AND NOT EXISTS (usage_active_user_counts …)
// AFTER:
DELETE FROM usage_tenants t WHERE t.purged_at IS NOT NULL AND t.purged_at < $1 -- now − SnapshotBillingHoldMonths (4)
  AND NOT EXISTS (usage_hourly …) AND NOT EXISTS (usage_active_user_counts …)
```
Test: `TestPrune_RetentionOnly` now keeps a tenant purged a month ago with no usage and still prunes one purged 6 months ago; it fails with the hold removed. Live: after the fix (the lost row restored as the fixed code keeps it), regenerate created charlie's final invoice under its kept name, billed at Card C (the month-start plan): INR 99.00 + GST 17.82 = 116.82, email "not delivered".
