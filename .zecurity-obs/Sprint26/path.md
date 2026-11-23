---
type: sprint
sprint: 26
title: Provider Dashboard — billing plans and invoicing (F34)
status: DRAFT pending sign-off by Barath — D-62 … D-68 and the answers to Q1–Q6 are proposals; no implementation until signed off
created: 2026-11-23
team:
  - Person 1 = Barath (M2): F34 — P1 plan cards (schema + API), P2 invoice generator and lifecycle
  - Person 2 = Sathiya (M1): X3 Sprint 25 follow-ups; F34 — P3 invoice API, P4 billing console
baseline: Sprint 25 stack (#244 docs, #245 U1, #246 U2, #247 U3, #248 U4; code complete, not merged); main cb441cc9
---

# Sprint 26 — Billing plans and invoicing

## Goal

Sprint 25 made each tenant's usage **measurable** (F32, F33). Sprint 26 makes it **billable**: the provider builds **plan cards** (limits, monthly price and included quantities per meter) that it can create and edit whenever it wants, and after each UTC month closes the controller turns the tenant's plan and its counted usage into one **invoice** per tenant. A super-admin reviews it, issues it (from then on it can't be changed and it has a number), and records it as paid or void. Sprint 26 **prices and invoices**. It collects no money, calculates no tax and **enforces nothing**.

| Feature | Today | Phases (owner) |
|---|---|---|
| **F34** Billing plans and invoicing | ⬜ Plans have limits but no price (D-43). The usage ledger and its reader exist (D-57 … D-60). | **P1** plan cards (Barath), **P2** invoice generator (Barath), **P3** invoice API (Sathiya), **P4** console (Sathiya) |
| Sprint 25 follow-ups | Sprint 25 acceptance run not yet done | **X3** (Sathiya) |

Tracker: `.zecurity-obs/Planning/Provider-Dashboard-Features.md`. **Binding decisions (after sign-off):** `docs/provider-dashboard-architecture-decisions.md`, amendment **2026-11-xx, billing (D-62 … D-68)**, on top of D-01 … D-61. Scope discussion: `Sprint26/Scope-Draft.md` (PR #249).

**Prerequisites:**
1. The Sprint 25 stack merged in order (#244 → #248).
2. The Sprint 24 acceptance run done (its findings are Sprint 25 X2).
3. The **Sprint 25 acceptance run** done (its findings are X3). F34 prices the ledger's numbers, so the ledger must pass its live check first.

---

## Decisions (Phase 0, proposed, pending sign-off)

The proposed decisions are below; the scope draft gives the reasons for each. Where a decision depends on a review question, the **recommended answer** is used as a working assumption and marked *(Qn)*; answered questions are marked ✔ (**Q1 answered 2026-11-23**). Before sign-off, Barath confirms or changes each one, and the phase files follow.

| ID | Decision | Phase |
|---|---|---|
| **D-62** | **Scope:** F34 plan cards and invoices (+ X3). **Not in scope:** collecting payment (no payment processor, no card data), calculating tax *(Q6)*, enforcing anything on non-payment, tenant-facing invoices or invoice email *(Q5)*. No agent change. A plan card without a price is **not billable**: its tenants get no invoice. | all |
| **D-63** | **Plan cards** ✔ *(Q1 answered 2026-11-23: editable plan cards)*: a plan is a **card** the provider creates and edits whenever it wants. A card has: the B1 part (name, description, the six quota limits of D-43/D-47) and a **price**: a **monthly base fee** and, for each meter it prices, an **included quantity**, a **unit size** and a **unit price** for usage above the included quantity. Billable meters are the D-57 meters; a meter the card leaves out is not billed. **Create** a card = a new plan with or without a price; **edit** a card = change any field, including the price and the included quantities, at any time (super-admin, reason, before/after audit). Every edit raises the card's **revision** and is kept in an append-only revision history. A custom deal is just a new card assigned to that tenant. A card without a price is **not billable**. | P1, P2, P4 |
| **D-64** | **Money:** **one currency for the provider** *(Q2)*, `BILLING_CURRENCY` (ISO 4217, set at start, e.g. `USD`). Every price and amount is a whole number of **minor units** (`BIGINT`); there is no floating point anywhere. Usage above the included quantity is rounded **up to whole units** per meter per invoice. Line amount = billable units × unit price, with an overflow check (an overflow refuses; it never wraps). | P1, P2 |
| **D-65** | **Plan change in the month** *(Q3)*: the plan the tenant has **at month close**, **as its card reads at that moment**, sets the base fee, included quantities and prices for the **whole month**. There is no proration. An edit to a card during the month therefore applies to that month's invoice; an edit after a draft exists applies when the draft is regenerated; an **issued** invoice never changes. For a live tenant that is `workspaces.plan_id`; for a tenant purged during the month it is the plan frozen at purge (`usage_tenants.plan_id`). | P2 |
| **D-66** | **Invoice lifecycle:** **draft** → **issued** → **paid** or **void**. A draft can be regenerated. An issued invoice can't be changed and gets a number from a **gap-free counter**, `INV-<year>-<6 digits>`. Void needs a reason. A paid invoice is final (refunds are out of scope). A correction is a void plus a new invoice, never an edit. Drafts are generated after the UTC month closes **plus a 6-hour settling delay**. Quantities are **counts only (D-59)**: hours the collector missed are billed as counted, and the invoice states how many there were. | P2, P3 |
| **D-67** | **Retention and purge** *(Q4)*: invoices and their lines are **financial records**. They are kept **8 years**, are **not deleted by the S4 purge** (**amends D-46** again) and are not pruned with the ledger's 25 months (D-58), because each line holds its own quantities and prices. A tenant purged during a month gets a final invoice for that month, named from `usage_tenants`. | P2 |
| **D-68** | **Who:** billing is tenant data, so it is **super-admin only** (D-40/D-60 pattern; relay-ops get 403 and see no billing pages). Card writes reuse the B1 plan actions (super-admin only); new `decide()` actions `invoice.read`, `invoice.write`. Every write carries a reason and a before/after audit (D-53 style). Reading one invoice is audited as `invoice.read`; the list writes one `invoice.fleet.read` row per request. **Amends D-30:** a card's price and included quantities (on plan create and update), and issuing, marking paid, voiding or regenerating invoices are console writes. A separate `billing` role is part of F19 (not this sprint). | P1, P3, P4 |

**Extends:** D-43 (plans become cards with a price; limits keep their rules), D-40/D-60 (billing hidden from relay-ops). **Amends:** D-30 (new console writes), D-46 (the purge keeps invoices). **Unchanged:** D-02, D-45 (suspension stays a manual action; unpaid invoices never suspend), D-47 (quotas unchanged), D-57 … D-61 (the ledger and usage alerts stay as they are).

---

## Team assignments

**Person 1, Barath (M2):** P1a → P1b → P2. Phase file: `Member1-Barath/Phases-P1-P2-Prices-Invoices.md`.
**Person 2, Sathiya (M1):** X3 → P3 → P4. Phase file: `Member2-Sathiya/Phases-X3-P3-P4-Billing-Console.md`.

**Both:**
- review each other's PRs;
- run the Sprint 25 acceptance before P1 starts;
- run the Sprint 26 acceptance at the end (live, on a reset lab DB, current `main`).

### Phases and order

| Phase | Owner | Feature | Depends on | Migration |
|---|---|---|---|---|
| **X3** Sprint 25 follow-ups | Sathiya | F32, F33 | the Sprint 25 acceptance run | none expected |
| **P1a** Billing schema | Barath | F34 | sign-off; Sprint 25 merged | `073_plan_cards.sql`, `074_invoices.sql` |
| **P1b** Plan card API | Barath | F34 | P1a merged | none |
| **P2** Invoice generator + lifecycle | Barath | F34 | P1a merged (P1b can run at the same time) | none |
| **P3** Invoice API | Sathiya | F34 | P1a merged; P2's `internal/billing` lifecycle functions merged before P3's write handlers | none |
| **P4** Billing console | Sathiya | F34 | P1b and P3 merged | none |

```
Barath:  P1a (schema: 073 + 074) ──► P1b (card API) ────────────────────┐
              │                └──► P2 (generator + lifecycle) ──┐       │
Sathiya: X3 ──┴──► P3 reads (against P1a's tables) ──► P3 writes ┴──► P4 (console)
```

- **P1a merges first**, on its own (schema only, with its constraint and trigger tests). The tables are the contract between Barath and Sathiya. P3's read endpoints are built and tested against rows their tests insert; they don't wait for the generator.
- **One lifecycle implementation:** issue, mark paid, void and regenerate live in `internal/billing` (P2). P3's handlers call those functions and add authz, audit and HTTP; they don't reimplement the state changes.
- **One usage reader:** P2 reads the month's quantities through `providerquery.ReadMonthUsage` (U2), never with its own SQL over `usage_hourly`.
- Migration numbers assume the Sprint 25 stack is merged (latest `072_usage_alert_kind.sql`). Recheck before P1a.

### Conflict zones (shared files)

| File / area | Who | Rule |
|---|---|---|
| `controller/internal/providerquery/usage.go` (U2 reader) | Barath (P2) | Read through `ReadMonthUsage`; if P2 needs a field it doesn't return, add it there (one reader for U3, U4 and P2). Don't change its existing output. |
| `controller/internal/usage/` (U1 collector, `FreezeTenant`) | Barath (P1a/P2) | Only: keep `usage_tenants.plan_id` current with the name and slug, and freeze it at purge. Nothing else changes. |
| `controller/internal/tenantlifecycle/purge.go` (S4) | Barath (P2) | The D-67 exception only: the purge never touches `invoices` / `invoice_lines` (they have no foreign key to `workspaces`). Nothing else changes. |
| Plans (B1): `controller/internal/plans/` (or wherever B1's handlers live), `Plans` console page | Barath (P1b), Sathiya (P4) | P1b extends B1's plan create and update with an optional `price` (one transaction, one audit row). Limits validation, default and assignment rules don't change. Deleting a plan still follows B1 (refused while assigned or default); its price goes with it, its revision history stays, and invoices keep their own snapshot. |
| `controller/cmd/server/main.go` | both | Own block per phase (month-close job, routes); rebase before merge. |
| `controller/internal/provider/authz.go` (`decide()`) | Sathiya (P3) | `invoice.read` / `invoice.write` (P3; cards reuse B1's plan actions): super-admin only by the existing rule. No addition to `relayOpsAlsoAllowed`. |
| `provider-console/src/api/api-surface.test.ts` | Sathiya (P4) | New reads and **new writes** added on purpose (D-68 amends D-30). |
| `TenantDetail.tsx`, `Plans` page, navigation (`roles.ts`, `Layout.tsx`) | Sathiya (P4) | Plans page as cards (create, edit, history), Invoices section on tenant detail, a super-admin-only Invoices page. |
| `controller/migrations/` | Barath | Reserved `073_`, `074_`; `[schema: reset DB]` in the PR title. |

---

## Rules (this sprint)

- Decisions **D-62 … D-68 are binding once signed off**, together with D-01 … D-61. If a phase needs another architectural choice, stop and ask; don't decide it in code.
- **Integers only:** money is in minor units (`BIGINT`); no `float`, no `NUMERIC` rounded in Go, no `math.Round`. Overage is rounded up to whole units. Overflow refuses.
- **Counts, not estimates (D-59):** invoice quantities come from the ledger through U2's reader; never from M1, never interpolated, never back-filled. Incomplete hours are stated, not filled in.
- **Exactly once:**
  - one non-void invoice per (tenant, month), enforced by a unique index;
  - regenerating a draft replaces it and never adds a second one;
  - issuing an invoice twice gives one number.
- **Immutable once issued:** after issue, a DB trigger refuses any change except the status moving to `paid` or `void` with their fields. Lines can't be changed. Drafts are the only thing that can be deleted or regenerated.
- **Gap-free numbering:** the invoice number comes from a counter row locked in the issue transaction, not from a Postgres `SEQUENCE` (rollbacks would leave gaps).
- **Prices are snapshots:** an invoice keeps the plan name, currency, the card's revision and each line's included quantity, unit and unit price. A later card edit or plan rename never changes an issued invoice.
- **Tenant boundary (D-68):** relay-ops get 403 on every price and invoice route and never see billing pages. Tenant-scoped reads always filter by `tenant_id`.
- **No enforcement (D-62):** no code path reads invoices or prices to refuse, slow, suspend or limit anything.
- **Purge (D-67):** invoices survive the purge; no tenant content beyond the invoice's own fields (tenant name and slug snapshot) is kept.
- **Single controller (D-02):** the month-close job is process-local; no cross-replica locking.
- No secrets or payment data anywhere: a "paid" reference is a free-text note (max 200 characters), never card or bank details.
- Schema changes go in new numbered files only; there is no migration framework; recreate the local DB (`docs/database-development.md`).
- Each phase ends with a **live check** recorded in `.zecurity-obs/Planning/Pending-Live-Tests.md` before F34 is ticked.
- **No commit, push, PR or merge without the product owner's explicit go** for that step.

## Test gates (each phase)

- **Controller:** `go build ./... && go vet ./...`, then the full `go test ./...` with the DB env (`PKI_TEST_DATABASE_URL`, `AUTH_TEST_VALKEY_URL`). 0 failures, and only the known hard-coded skips (a skipped DB test is a failed case).
- **Provider console:** `npm test`, `npm run lint`, `npm run build`.
- **Mutation checks** for each new guard:
  - an amount computed with floating point, or overage rounded down;
  - an issued invoice or its line changed (trigger removed);
  - a second non-void invoice for the same tenant and month;
  - a gap or a duplicate in invoice numbers (concurrent issue);
  - a quantity read from M1 or from SQL outside U2's reader;
  - a card edit changing an issued invoice;
  - a card edit not recorded in the revision history;
  - the purge deleting invoices;
  - relay-ops reading prices or invoices (must be 403);
  - an unpriced plan producing an invoice;
  - a plan change mid-month billed at the old plan (D-65);
  - incomplete hours not stated on the invoice.
- Phase-specific tests are in the phase files.

## Acceptance (end of sprint)

Live run on a **reset lab DB with current `main`**. Record the Git SHA and the latest migration, use traffic probes, keep the logs. Use a test clock (or a short `BILLING_SETTLE_DELAY` and a past month) for the month close.

- **Plan cards:**
  - Create two cards: card A prices relay bytes per GB with an included quantity, card B prices active users per user. Create a third, custom card for one tenant and assign it.
  - Edit card A's price and included quantity mid-month: the month's draft (after regenerate) uses the edited card; an invoice issued earlier is unchanged; the revision history shows both versions.
  - Each card create and edit is audited with a reason and before/after.
- **Invoices:**
  - Two tenants (A over its relay-byte included quantity, B with three active users) and a known download. After the month close, each draft's lines match U3's usage report and the cards exactly (amounts checked by hand).
  - A tenant on an unpriced plan gets no invoice.
  - The controller stopped for 5 minutes in the month: the invoice states the incomplete hours and bills what was counted.
  - A tenant whose plan changed mid-month is billed the whole month at the plan it held at close.
  - Regenerate a draft twice: still one invoice.
  - Issue it: it gets the next number, and a direct SQL `UPDATE` of the issued invoice is refused.
  - Mark it paid. Void another invoice with a reason; it stays listed as void. Regenerate creates a new draft for the voided tenant and month.
  - Purge a tenant mid-month: its final invoice is generated with its kept name, and its earlier invoices survive the purge.
- **Console:**
  - The Plans page as cards (create, edit, revision history), the Invoices page (month, status, totals), the invoice detail with actions, the tenant's Invoices section, CSV/JSON export and the print view.
  - Relay-ops get 403 on the API and see no billing pages.
  - Reads write `invoice.read` / `invoice.fleet.read`; writes write before/after with the reason.
- **X3:** each follow-up from the Sprint 25 acceptance run checked live.

Tracker: F34 ticked only after its live check.

## Checklist

- [ ] Sprint 25 stack merged (#244 → #248); Sprint 25 acceptance run (both) done; follow-ups recorded as X3; tracker F32, F33 ticked
- [ ] Phase 0: Q1–Q6 answered (Q1 ✔ 2026-11-23); D-62 … D-68 decided and written into the Decision Record (amendment billing)
- [ ] Documents reviewed (scope draft, Sprint 26 path, phase files, Decision Record amendment) and signed off by Barath; implementation may start
- [ ] X3: Sprint 25 follow-ups (Sathiya)
- [ ] P1a: Billing schema `073_plan_cards`, `074_invoices` (Barath)
- [ ] P1b: Plan card API (Barath)
- [ ] P2: Invoice generator and lifecycle (Barath)
- [ ] P3: Invoice API (Sathiya)
- [ ] P4: Billing console (Sathiya)
- [ ] Sprint acceptance run (both); tracker F34

## Post-Sprint Fixes

_None yet._

## Out of scope

- Payment collection (Stripe or any processor), stored payment methods, dunning, automatic suspension or any enforcement on non-payment or usage.
- Tax calculation and tax IDs; multi-currency.
- Tenant-facing billing: invoices or usage in the admin UI, and invoice email to tenants.
- Discounts, coupons, credits, trials, annual plans. (A custom price for one tenant is done with its own card, not an override.)
- Refunds and credit notes (a paid invoice is final this sprint).
- Server-side PDF rendering (the console prints its own view).
- Direct-path traffic metering.
- F19 (incl. a `billing` role), F20, F22, F28/F29, F31, F35, F37, F39, F40.
