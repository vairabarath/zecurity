---
type: sprint-scope-draft
sprint: 26
title: Provider Dashboard — billing plans and invoicing (F34)
status: SIGNED OFF 2026-11-23 (by Sathiya, overriding the product-owner sign-off step; Barath reviews in PR #249) — superseded by Sprint26/path.md, the phase files and the Decision Record amendment 2026-11-23 (billing, D-62 … D-70); kept as the scope discussion record. All six questions answered 2026-11-23 (see "Answered"); the answers changed D-64, D-65, D-67, D-68 and added D-69 (tax) and D-70 (who), so the proposals below are partly outdated
baseline: Sprint 25 stack (PR #244 docs, #245 U1, #246 U2, #247 U3, #248 U4; code complete, not merged); main cb441cc9
---

# Sprint 26 — scope draft

> **Status: signed off 2026-11-23; kept as the discussion record.** The binding versions are `Sprint26/path.md`, the phase files and the Decision Record amendment **2026-11-23, billing (D-62 … D-70)**. The proposals below are from before the answers; where they differ, the binding documents win.
>
> **Prerequisites before any Sprint 26 code:** the Sprint 25 stack (#244 → #248) merged in order, the Sprint 24 acceptance run done (its findings are Sprint 25 X2), and the **Sprint 25 acceptance run** done (its findings become Sprint 26 **X3**). F34 prices the ledger's numbers, so the ledger must be live-checked first.

## Goal

Sprint 25 made each tenant's usage **measurable** (F32, F33). Sprint 26 makes it **billable**: the provider gives each plan a price, and at each UTC month close the controller turns the month's plan and usage into an **invoice** per tenant. The provider reviews it, issues it and records it as paid or void.

- **Plan cards:** each plan (B1, D-43) becomes a **card**: limits plus a monthly base fee and, per meter, an included quantity and a unit price for usage above it. The provider **creates new cards and edits the price and included quantities whenever it wants**. Every edit is kept in a revision history, and an issued invoice keeps the prices it used.
- **Invoices:** one per tenant per UTC month, built from the ledger (U2's reader, D-59 counts only). Draft, then issued (immutable, numbered in sequence), then paid or void. Each invoice says which of the month's hours were incomplete.
- **Provider console:** the Plans page as cards (create, edit, history), an Invoices page, an invoice detail view, a tenant's invoices on tenant detail, and an export (CSV, JSON and a printable view).

**Billing records, not payments.** Sprint 26 **prices and invoices**. It does **not** collect money (no payment processor, no stored cards). It calculates **no tax**. It never **enforces**: an unpaid invoice never suspends, throttles or limits a tenant (suspension stays a manual D-45 action). D-56's rule "no agent change" still holds.

| Feature | Today | Proposed phases | Size |
|---|---|---|---|
| **F34** Billing plans and invoicing | ⬜ Plans have limits only, no price (D-43). The ledger counts usage per tenant per hour and month (`usage_hourly`, `usage_active_user_counts`, `usage_tenants`; D-57, D-58), and U2 reads it (`ReadTenantUsage`, `ReadMonthUsage`). | **P1** plan cards, **P2** invoice generator, **P3** invoice API, **P4** console | 🟡 Medium to 🔴 Large (two migrations, one background job, new console writes; no agent change) |
| **Sprint 25 follow-ups** | Sprint 25 acceptance run not yet done | **X3** (whatever the run finds) | 🟢 Small |

## What already exists (reuse, don't rebuild)

| Building block | Where | Used by |
|---|---|---|
| Plans with six limit keys, one default, assignment with reason and before/after audit | `062_tenant_plans.sql`, `plans`, `workspaces.plan_id`, action `tenant.plan.assign` (D-43) | P1: a plan becomes a card with a price. P2: the plan a tenant is billed for |
| Usage ledger: hourly meters, collector minutes (completeness), monthly active users, tenant snapshot that survives a purge | `071_usage.sql` (D-57, D-58, D-59) | P2: invoice quantities |
| Usage reader (one reader, used by U3 and U4): tenant window, month for the whole fleet | `providerquery/usage.go` (`ReadTenantUsage`, `ReadMonthUsage`) | P2 reads month totals and completeness through it. No second usage query |
| Tenant snapshot for purged tenants (id, name, slug, plan name) | `usage_tenants` (D-58) | P2: a final invoice for a tenant purged mid-month |
| Append-only table pattern (UPDATE/DELETE/TRUNCATE refused, SQLSTATE 42501) | `051_provider_audit_append_only.sql` (F17) | P2: issued invoices and their lines can't be changed |
| Provider audit with reason and before/after | `provider.WithChange` (F17, D-53) | P1, P3 writes |
| Background job on the controller (single instance, D-02) | U1b collector, A1 evaluator | P2 month-close job |
| Client-side CSV/JSON export (formula cells neutralised, row cap) | console audit export (F38), usage export (U3) | P4 export |
| Usage alerts (warn only) | `tenant.usage_over` (D-61) | Unchanged; can be set at a plan's included quantity |

## Proposed phases

### X3 — Sprint 25 follow-ups (first, small)
Run the Sprint 25 acceptance (both, reset lab DB, current `main`) before P1 starts. Anything it finds lands here, as X1 and X2 did. Then tick F32 and F33 in the tracker.

### P1 — Plan cards (controller + migration `073_plan_cards`)
- A **plan card** is the B1 plan (name, description, six limits) plus an optional **price**: `plan_prices (plan_id PK, currency, base_fee_minor BIGINT, meters JSONB, updated_at, updated_by)`, one row per plan.
  - `meters`: one entry per billable meter (D-63): `{included: BIGINT, unit_price_minor: BIGINT, unit: BIGINT}`. Example: relay bytes are priced **per GB** (`unit` = 10⁹ bytes) with 50 GB included. Active users are priced **per user** with 5 included. A meter left out is not billed. Usage above the included quantity is rounded **up to whole units** (D-64).
  - Money is **integers in minor units** (cents or paise). No floats anywhere.
- **Create and edit any time:** a new card is a new plan with or without a price. An edit can change the price, any included quantity, the limits or the name, in one transaction. Each edit raises `plans.revision` and adds a row to the append-only `plan_card_revisions` history. A custom deal is a new card assigned to that tenant.
- A card **without a price** is not billable: its tenants get no invoice (D-62). The Free and Legacy seeds start with no price.
- **API:** B1's plan create and update gain an optional `price`; `GET /provider/plans/{id}/revisions` lists the history. Super-admin only (B1's plan actions), reason, audited before/after.
- **Tests:** DB-backed:
  - create and edit (price only, included quantity only, limits and price together), one revision per edit;
  - a failed edit changes nothing;
  - integer overflow guard;
  - unknown meter refused, negative values refused;
  - a card with no price is not billable.

### P2 — Invoice generator (controller + migration `074_invoices`)
- **Tables:**
  - `invoices (id, number, tenant_id, month, status draft|issued|paid|void, currency, plan_name, plan_revision, subtotal_minor, incomplete_hours, generated_at, issued_at, issued_by, paid_at, void_reason, …)`
  - `invoice_lines (invoice_id, kind base|meter, meter, quantity, included, billable_units, unit_price_minor, amount_minor)`
  - Unique `(tenant_id, month)` for non-void invoices.
- **Month-close job:** runs on the controller (D-02) after the UTC month ends plus a **settling delay** (proposed 6 hours, so the last hour's collector minutes and relay deltas are flushed). For each tenant on a priced plan it creates or **regenerates** a **draft**. This is idempotent: a re-run replaces the draft and never creates a second invoice.
  - Quantities come from **U2's reader** for the month. **Counts only, never estimated (D-59):** incomplete hours are billed as counted, and the invoice states how many hours were incomplete.
  - The base fee follows the plan-change rule in D-65.
- **Issue:** a super-admin issues a draft. It gets the next number from a gap-free sequence (D-66) and becomes **immutable**: a trigger refuses UPDATE/DELETE on issued invoices and their lines, except the status move to `paid` or `void`.
  - **Paid** records a date and a reference (a note, never card data).
  - **Void** needs a reason. A corrected invoice is a new invoice: a voided invoice is never edited.
- **Purged tenants (D-67):** a tenant purged during the month gets a final draft for that month, named from `usage_tenants`. Invoices are **financial records**: they are not deleted by the purge (amends D-46 again) and are kept beyond the ledger's 25 months (D-58), because their lines hold the quantities.
- **Tests:** DB-backed: month boundaries in UTC, included quantity and rounding, a draft regenerated twice is still one invoice, issued invoice unchangeable (mutation check), numbering gap-free under concurrent issue, purged tenant final invoice, incomplete hours stated, plan change mid-month (D-65), tenant on an unpriced plan gets nothing.

### P3 — Invoice API (controller)
- `GET /provider/invoices?month=&status=&tenant_id=&cursor=`: the fleet list, keyset-paged. `GET /provider/invoices/{id}`: the invoice with its lines.
- Writes:
  - `POST /provider/invoices/{id}/issue` (reason optional)
  - `POST /provider/invoices/{id}/mark-paid` (date and reference, reason)
  - `POST /provider/invoices/{id}/void` (reason required)
  - `POST /provider/invoices/regenerate?month=` (drafts only)
- Every write is super-admin only and audited before/after (D-53 style).
- **Invoices are tenant data** (D-60 pattern): super-admin only, `decide()` actions `invoice.read` and `invoice.write`, and relay-ops get **403**. A read of one invoice is audited `invoice.read`. The list writes one `invoice.fleet.read` row per request.

### P4 — Billing console (provider console)
- **Plans page as cards:** create a card; edit its price, included quantities and limits; duplicate it as a custom card; see its revision history (reason on every change).
- **Invoices page** (super-admin navigation): by month and status, totals per currency. Detail view with lines, incomplete hours, status history and actions (issue, mark paid, void with reason).
- **Tenant detail → Invoices:** the tenant's invoices. Purged tenants keep their name and a "Deleted" badge (as in U3).
- **Export:** CSV and JSON of a month's invoices, plus a **printable invoice view** (browser print to PDF). No server-side PDF.
- New writes go into `api-surface.test.ts` on purpose. D-30 is amended (D-68) to add price-book and invoice writes.

## Decisions to make first (Phase 0, proposed D-62 … D-68)

| Proposed ID | Question | Recommendation |
|---|---|---|
| **D-62** | Scope of Sprint 26 | **F34** plan cards and invoices (+ X3). **No payment collection** (no payment processor, no card data), **no tax calculation**, **no enforcement** on non-payment, **no tenant-facing invoices**. No agent change. A plan without a price is not billable. |
| **D-63** | Price model | Plans become **cards** the provider creates and edits whenever it wants: limits plus a **monthly base fee** and, per meter, an **included quantity** and a **unit price above it** (overage). Billable meters: the D-57 meters (`relay_bytes_to_clients`, `relay_bytes_to_connectors`, `session_minutes`, `connector_minutes`, `shield_minutes`, `active_users`). Each card chooses which meters it prices. Every edit is kept in a revision history. The card as it reads at month close bills the month (D-65), and issued invoices never change. ✔ *Q1 answered 2026-11-23: editable plan cards are the right shape.* |
| **D-64** | Money and rounding | **One currency per provider** (set once in config, e.g. `BILLING_CURRENCY=USD`). Integer **minor units** throughout. Overage is rounded **up to whole units** per meter per invoice. Line amount = billable units × unit price. No floating point. *Open question Q2 (which currency).* |
| **D-65** | Plan change in the month | The plan assigned at **month close**, as its card reads then, sets the base fee, included quantities and prices for the **whole month** (no proration). A card edit during the month applies to that month. Simple, and the plan assignment audit already shows the history. *Alternative:* prorate the base fee by day, which needs a plan-assignment history table. *Open question Q3.* |
| **D-66** | Invoice lifecycle | Draft (regenerable) → **issued** (immutable, number from a **gap-free sequence**, e.g. `INV-2026-000123`) → **paid** or **void** (reason). Corrections are a void plus a new invoice, never an edit. Generated after the UTC month close plus a **6-hour settling delay**. Incomplete hours are billed as counted and stated on the invoice (D-59). |
| **D-67** | Retention and purge | Invoices and their lines are **financial records**. They are kept **N years** (proposal: 8) and are **not deleted by the S4 purge** (amends D-46). They are not pruned with the ledger (D-58), because lines hold their own quantities. A tenant purged in the month gets a final invoice for it, named from `usage_tenants`. *Open question Q4 (retention period: a legal and operations choice owned by the product owner).* |
| **D-68** | Who, and console writes | Super-admin only (tenant data, D-40/D-60 pattern; relay-ops 403). Card writes reuse B1's plan actions; new actions `invoice.read`, `invoice.write`. Writes carry a reason and before/after audit (D-53). **Amends D-30:** card prices and included quantities, and invoice issue, mark paid, void and regenerate are console writes. A separate `billing` role is F19 (not this sprint). |

**Answered (2026-11-23), all six:**
1. **Q1 price model:** editable **plan cards** are the right shape (create a card; edit its price, included quantities and limits any time; the card at month close prices the month) → D-63.
2. **Q2 currency:** **editable**, default **USD** (United States). It can be set to e.g. **INR** for India. Changing the country or currency **switches every plan card** to that currency (amounts aren't converted) → D-64.
3. **Q3 plan change mid-month:** the **month-start plan** bills the whole month and the new plan bills **from the next month**. The month-start plan is preserved (`tenant_billing_plans`). Limits still change immediately → D-65.
4. **Q4 retention:** **8 years by default, editable** (1–30) → D-67.
5. **Q5 delivery:** also **email invoices to the tenant's admins** on issue (and a void notice). This is an editable setting, default on → D-68.
6. **Q6 tax:** **no tax by default** ("Tax not included"). **Editable:** one tax rate and label (e.g. GST 18 %) → D-69.

**Changed after sign-off (2026-11-24, Sathiya):**
- **Card edits apply from the next month (D-63).** The month is billed with the card **as it was at the start of the month**, not as it reads at month close. Example: Starter ₹499 / 50 GB included / ₹2 per extra GB, edited on the 15th to ₹599 / 100 GB / ₹1 → that month is billed ₹499 / 50 GB / ₹2; the new terms bill from the next month.
- **Minutes meters are an editable choice per card**; **session minutes are not billed by default** (a new card starts with no meter billed). Q-1 from the Sprint 25 run: a session counts while valid, also while the device sleeps, so billing it is the provider's choice.

## Suggested split (two members, as in Sprint 25)

| Member | Phases | Order |
|---|---|---|
| **Barath (M2)** | **P1** plan cards (schema + API), **P2** invoice generator (builds on his U1 ledger) | P1a schema first (`073_` and `074_` together: the contract with P3/P4), then P1b API and P2 |
| **Sathiya (M1)** | **X3**, **P3** invoice API, **P4** console (builds on U2/U3) | X3 → P3 (after P2's `074_` schema) → P4 |

The contract between the two members is the `plan_prices` and `invoices` / `invoice_lines` schema. Proposal: Barath merges `073_` and `074_` (schema only, with constraint tests) first. P3 is then built and tested against rows its tests insert, while the generator is still in progress (as U1a/U2 worked in Sprint 25).

**Migrations (to reserve):** `073_plan_cards` (P1), `074_invoices` (P2). Latest after the Sprint 25 stack: `072_usage_alert_kind`. Check `ls controller/migrations | tail -3` before adding. No migration framework. `[schema: reset DB]`.

**Conflict zones:**
- `controller/internal/providerquery/usage.go`: P2 reads through it and must not add a second usage query.
- `controller/internal/tenantlifecycle/purge.go`: the D-67 exception only.
- `controller/cmd/server/main.go`: job and routes.
- `controller/internal/provider/authz.go`: new actions.
- Plans handlers (B1): P1 extends create and update with the card's price; limits rules don't change.
- `provider-console/src/api/api-surface.test.ts`: new writes, on purpose.
- `TenantDetail.tsx`, the Plans page, and the provider navigation (`roles.ts`, `Layout.tsx`).

## Not in Sprint 26 (proposed)
- **Payment collection** (Stripe or any processor), dunning, automatic suspension for non-payment. Any enforcement on usage or billing.
- **Tax** calculation and tax IDs (unless Q6 says otherwise). **Multi-currency.**
- **Tenant-facing billing** (invoices and usage in the admin UI, invoice email to tenants; unless Q5 says otherwise).
- Discounts, coupons, credits and trials. Annual plans. (A custom price for one tenant is its own card, not an override.)
- Direct-path traffic metering (connector counters, agent change).
- F19 roles (incl. a `billing` role), F20, F22, F28/F29, F31, F35, F37, F39, F40.

## Test gates (as in Sprint 25)
- Build, vet and the full Go suite with every DB-backed test running (a skipped DB test is a failed case). Console tests, lint and build.
- **Mutation checks:**
  - a float or rounding-down in an amount;
  - an issued invoice changed;
  - a second invoice for the same tenant and month;
  - a gap or duplicate in invoice numbers;
  - usage taken from M1 instead of the ledger;
  - the purge deleting invoices;
  - relay-ops reading invoices (must be 403);
  - an unpriced plan producing an invoice.
- **Live acceptance on the lab:**
  - Two tenants on two priced plans, known usage in a month (relay bytes over the included quantity for one, under it for the other). After the month close (test clock), each draft's lines match U3's usage report and the cards exactly.
  - A tenant on an unpriced plan gets no invoice.
  - The controller stopped for 5 minutes in the month: the invoice states the incomplete hours and bills what was counted.
  - Regenerate a draft twice: still one invoice. Issue it: number assigned. An attempt to change it is refused. Mark paid. Void another with a reason: it stays visible as void.
  - Create a card and a custom card. Edit a card's price and included quantity mid-month: the month's draft (after regenerate) uses the edited card, an issued invoice doesn't change, and the history shows both revisions.
  - Purge a tenant mid-month: its final invoice is generated with its kept name. Its invoices survive the purge.
  - Console: plan cards (create, edit, history), Invoices page, detail, tenant Invoices, export and print. Relay-ops get 403 and see no billing pages.

## Next steps
1. ~~Answer Q1–Q6 and sign off~~: done 2026-11-23.
2. ~~Write `Sprint26/path.md`, the phase files, the Decision Record amendment (D-62 … D-70), the `CLAUDE.md` switch and the Session Log entry~~: done 2026-11-23.
3. Before P1 starts: merge the Sprint 25 stack, run the Sprint 25 acceptance run and record its findings as X3.
