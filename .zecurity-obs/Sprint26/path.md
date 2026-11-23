---
type: sprint
sprint: 26
title: Provider Dashboard — billing plans and invoicing (F34)
status: decided and signed off 2026-11-23 (sign-off given by Sathiya, who instructed that it override the product-owner sign-off step; Barath's review in PR #249); implementation may start once the Sprint 25 stack is merged (X3 after the Sprint 25 acceptance run, P3 after P1a)
created: 2026-11-23
team:
  - Person 1 = Barath (M2): F34 — P1 plan cards and billing settings (schema + API), P2 invoice generator and lifecycle
  - Person 2 = Sathiya (M1): X3 Sprint 25 follow-ups; F34 — P3 invoice API and tenant invoice email, P4 billing console
baseline: Sprint 25 stack (#244 docs, #245 U1, #246 U2, #247 U3, #248 U4; code complete, not merged); main cb441cc9
---

# Sprint 26 — Billing plans and invoicing

## Goal

Sprint 25 made each tenant's usage **measurable** (F32, F33). Sprint 26 makes it **billable**:
- **Plan cards.** The provider builds **plan cards** (limits, a monthly price, and per meter an included quantity and a price above it) and can create and edit them whenever it wants.
- **Billing settings.** The provider sets its **billing country and currency** (default United States / USD), **tax** (default none) and **invoice retention** (default 8 years), and whether invoices are **emailed to tenants** (default on).
- **Invoices.** After each UTC month closes, the controller turns each tenant's month-start plan and its counted usage into one invoice. A super-admin reviews it and issues it; from then on it can't be changed, it has a number, and it is emailed to the tenant's admins. The super-admin then records it as paid or void.

Sprint 26 **prices, invoices and notifies**. It collects no money and **enforces nothing**.

| Feature | Today | Phases (owner) |
|---|---|---|
| **F34** Billing plans and invoicing | ⬜ Plans have limits but no price (D-43). The usage ledger and its reader exist (D-57 … D-60). | **P1** plan cards + billing settings (Barath), **P2** invoice generator (Barath), **P3** invoice API + tenant email (Sathiya), **P4** console (Sathiya) |
| Sprint 25 follow-ups | Sprint 25 acceptance run not yet done | **X3** (Sathiya) |

Tracker: `.zecurity-obs/Planning/Provider-Dashboard-Features.md`. **Binding decisions:** `docs/provider-dashboard-architecture-decisions.md`, amendment **2026-11-23, billing (D-62 … D-70)**, on top of D-01 … D-61. Scope discussion: `Sprint26/Scope-Draft.md` (PR #249).

**Prerequisites:**
1. The Sprint 25 stack merged in order (#244 → #248).
2. The Sprint 24 acceptance run done (its findings are Sprint 25 X2).
3. The **Sprint 25 acceptance run** done (its findings are X3). F34 prices the ledger's numbers, so the ledger must pass its live check first.

---

## Decisions (Phase 0, done 2026-11-23)

All six review questions were answered on 2026-11-23 (marked ✔). The full text is in the Decision Record.

| ID | Decision | Phase |
|---|---|---|
| **D-62** | **Scope:** F34 plan cards, billing settings and invoices, emailed to tenants (+ X3). **Not in scope:** collecting payment (no payment processor, no card data), enforcing anything on non-payment, tenant-facing billing pages in the admin UI. No agent change. A plan card without a price is **not billable**: its tenants get no invoice. | all |
| **D-63** | ✔ Q1 **Plan cards.** A plan is a **card** the provider creates and edits whenever it wants. It has the B1 part (name, description, the six limits of D-43/D-47) and an optional **price**: a **monthly base fee**, and for each meter it prices an **included quantity**, a **unit size** and a **unit price** for usage above the included quantity. Billable meters are the D-57 meters. Every edit raises the card's **revision** and is kept in an append-only history. A custom deal is a new card assigned to that tenant. The card **as it reads at month close** prices the month; issued invoices never change. | P1, P2, P4 |
| **D-64** | ✔ Q2 **Country and currency.** A provider-wide **billing country and currency**, **editable**, default **United States / USD** (for example India / INR). Choosing a country fills in its currency, and the currency can still be set on its own. **Changing the currency switches every plan card to it** in one transaction (each card gets a revision). Amounts are **not converted**: the console warns and the provider reviews each card's prices. Drafts take the new currency when regenerated; issued invoices keep theirs. Money is whole **minor units** (`BIGINT`), using the currency's exponent, with no floating point. Usage above the included quantity is rounded **up to whole units**. Overflow refuses; it never wraps. | P1, P2, P4 |
| **D-65** | ✔ Q3 **Plan change in the month.** A tenant is billed for the **whole month at the plan it had at the start of the month**. A plan changed mid-month is billed **from the next month**. The month-start plan is **preserved**: the first plan change in a month records the plan it replaces in `tenant_billing_plans (tenant, month, plan)`, in the same transaction as the change. A tenant created mid-month starts on its creation plan. Limits (D-43/D-47) still change **immediately**; only billing waits for the next month. There is no proration. | P1, P2 |
| **D-66** | **Invoice lifecycle.** **Draft** (can be regenerated) → **issued** → **paid** or **void**. An issued invoice can't be changed and gets a number from a **gap-free counter**, `INV-<year>-<6 digits>`. Void needs a reason. A paid invoice is final. A correction is a void plus a new invoice. Drafts are generated after the UTC month closes **plus a 6-hour settling delay**. Quantities are **counts only (D-59)**: hours the collector missed are billed as counted, and the invoice states how many there were. | P2, P3 |
| **D-67** | ✔ Q4 **Retention and purge.** Invoices are **financial records**. They are kept for an **editable retention period, default 8 years** (1–30), then pruned by a privileged function (audited). They are **not deleted by the S4 purge** (**amends D-46**) and not pruned with the ledger (D-58). A tenant purged during a month gets a final invoice, named from `usage_tenants`. | P2 |
| **D-68** | ✔ Q5 **Tenant email.** When an invoice is **issued**, it is **emailed to the tenant's active admins**: an HTML and plain-text body with the invoice and its lines, and no link into the provider console. When it is **voided**, a void notice is sent. Sending is an **editable setting, default on**. Delivery goes through a queue with retries, at most once per (invoice, kind), and a send failure never blocks issuing. A super-admin can **resend**. A tenant with no admin (e.g. purged) is recorded as "not delivered". | P3 |
| **D-69** | ✔ Q6 **Tax.** **Default: no tax**; the invoice says "Tax not included". **Editable:** one provider-wide **tax rate** (basis points, 0–10000) and **label** (e.g. "GST", "VAT"), applied to the subtotal and rounded half up in integers. The invoice stores the rate, label, tax and total. A change applies at month close or regenerate; issued invoices never change. There are no tax IDs and no per-country or per-tenant tax rules. | P1, P2, P4 |
| **D-70** | **Who.** Billing is tenant data, so it is **super-admin only** (D-40/D-60 pattern; relay-ops get 403 and see no billing pages). Card writes reuse the B1 plan actions. New actions: `billing.settings.write`, `invoice.read`, `invoice.write`. Every write carries a reason and a before/after audit (D-53 style). Reading one invoice is audited as `invoice.read`; the list writes one `invoice.fleet.read` row per request. **Amends D-30:** card prices, billing settings, and invoice issue, mark paid, void, regenerate and resend are console writes. | P1, P3, P4 |

**Extends:** D-43 (plans become priced cards; limit rules unchanged), D-40/D-60 (billing hidden from relay-ops). **Amends:** D-30 (new console writes), D-46 (the purge keeps invoices). **Unchanged:** D-02, D-45 (an unpaid invoice never suspends), D-47 (quotas), D-57 … D-61 (the ledger and usage alerts).

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
| **P1a** Billing schema | Barath | F34 | Sprint 25 merged | `073_plan_cards.sql`, `074_invoices.sql` |
| **P1b** Plan card + billing settings API | Barath | F34 | P1a merged | none |
| **P2** Invoice generator + lifecycle | Barath | F34 | P1a merged (in parallel with P1b) | none |
| **P3** Invoice API + tenant email | Sathiya | F34 | P1a merged; P2's lifecycle functions merged before P3's write handlers | none |
| **P4** Billing console | Sathiya | F34 | P1b and P3 merged | none |

```
Barath:  P1a (schema: 073 + 074) ──► P1b (cards + settings API) ────────┐
              │                └──► P2 (generator + lifecycle) ──┐       │
Sathiya: X3 ──┴──► P3 reads (against P1a's tables) ──► P3 writes + email ┴──► P4 (console)
```

- **P1a merges first**, on its own (schema only, with its constraint and trigger tests). It is the contract between Barath and Sathiya.
- **One lifecycle implementation:** issue, mark paid, void and regenerate live in `internal/billing` (P2). P3's handlers and the email enqueue call those functions.
- **One usage reader:** P2 reads quantities through `providerquery.ReadMonthUsage` (U2), never with its own SQL over `usage_hourly`.
- **One billing-plan rule:** `billing.PlanForMonth(tenant, month)` (P1b) gives the `tenant_billing_plans` row if there is one, otherwise the current plan (live tenant) or the frozen plan (purged tenant). P2 uses only this.
- Migration numbers assume the Sprint 25 stack is merged (latest `072_usage_alert_kind.sql`). Recheck before P1a.

### Conflict zones (shared files)

| File / area | Who | Rule |
|---|---|---|
| `controller/internal/providerquery/usage.go` (U2 reader) | Barath (P2) | Read through `ReadMonthUsage`. If P2 needs another field, add it there and leave the existing output unchanged. |
| `controller/internal/usage/` (U1 collector, `FreezeTenant`) | Barath (P1b) | Only: keep `usage_tenants.plan_id` current and freeze it at purge. |
| B1 plan assignment (the `tenant.plan.assign` path) | Barath (P1b) | In the same transaction as the change: insert the replaced plan into `tenant_billing_plans` for the current UTC month, `ON CONFLICT DO NOTHING` (D-65). Assignment rules and limits behaviour don't change. |
| B1 plan create and update, `Plans` console page | Barath (P1b), Sathiya (P4) | Extended with an optional `price` (one transaction, one audit row, one revision). Limits validation, default and delete rules don't change. |
| `controller/internal/tenantlifecycle/purge.go` (S4) | Barath (P2) | The D-67 exception only: the purge never touches `invoices`, `invoice_lines` or `invoice_emails` (no foreign key to `workspaces`). |
| Email sender (F14/A2) | Sathiya (P3) | Reuse the existing SMTP sender; add an invoice template. No new mail stack. |
| `controller/cmd/server/main.go` | both | Own block per phase (month-close job, email queue, routes); rebase before merge. |
| `controller/internal/provider/authz.go` (`decide()`) | Barath (P1b), Sathiya (P3) | `billing.settings.write` (P1b), `invoice.read` / `invoice.write` (P3): super-admin only by the existing rule. No addition to `relayOpsAlsoAllowed`. |
| `provider-console/src/api/api-surface.test.ts` | Sathiya (P4) | New reads and **new writes** added on purpose (D-70 amends D-30). |
| `TenantDetail.tsx`, `Plans` page, navigation (`roles.ts`, `Layout.tsx`) | Sathiya (P4) | Plans as cards, Billing settings, Invoices page, tenant Invoices section (all super-admin only). |
| `controller/migrations/` | Barath | Reserved `073_`, `074_`; `[schema: reset DB]` in the PR title. |

---

## Rules (this sprint)

- Decisions **D-62 … D-70 are binding**, together with D-01 … D-61. If a phase needs another architectural choice, stop and ask; don't decide it in code.
- **Integers only:**
  - money is in minor units (`BIGINT`); no `float`, and no `NUMERIC` rounded in Go;
  - overage is rounded up to whole units; tax is rounded half up in integer arithmetic;
  - overflow refuses.
- **Counts, not estimates (D-59):** invoice quantities come from the ledger through U2's reader; never from M1, never interpolated or back-filled. Incomplete hours are stated.
- **Month-start plan (D-65):** billing uses `PlanForMonth` only. A mid-month plan change never changes the current month's invoice.
- **Exactly once:**
  - one non-void invoice per (tenant, month), enforced by a unique index;
  - regenerating replaces a draft and never adds a second one;
  - one number per issued invoice;
  - one email per (invoice, kind) unless a super-admin resends.
- **Immutable once issued:** after issue, a DB trigger refuses every change except the status moving to `paid` or `void` with their fields. Lines can't be changed. Only the retention prune function may delete issued invoices.
- **Gap-free numbering:** numbers come from a counter row locked in the issue transaction, not from a `SEQUENCE`.
- **Snapshots:** an invoice keeps the tenant name and slug, the plan name and card revision, the currency, the tax rate and label, and each line's included quantity, unit and unit price. Later card edits, currency changes, tax changes or renames never change an issued invoice.
- **Tenant boundary (D-70):** relay-ops get 403 on every price, settings and invoice route and never see billing pages. Tenant-scoped reads always filter by `tenant_id`.
- **Tenant email (D-68):**
  - it goes only to the tenant's own active admins;
  - it carries the invoice content only: no secrets, no console links, no other tenant's data;
  - the recipient addresses are not written to the provider audit (only the count).
- **No enforcement (D-62):** no code path reads invoices, prices or tax to refuse, slow, suspend or limit anything.
- **Single controller (D-02):** the month-close job and the email queue are process-local.
- No payment data: a "paid" reference is a free-text note (max 200 characters), never card or bank details.
- Schema changes go in new numbered files only; there is no migration framework; recreate the local DB.
- Each phase ends with a **live check** recorded in `.zecurity-obs/Planning/Pending-Live-Tests.md` before F34 is ticked.
- **No commit, push, PR or merge without the product owner's explicit go** for that step.

## Test gates (each phase)

- **Controller:** `go build ./... && go vet ./...`, then the full `go test ./...` with the DB env (`PKI_TEST_DATABASE_URL`, `AUTH_TEST_VALKEY_URL`). 0 failures, and only the known hard-coded skips (a skipped DB test is a failed case).
- **Provider console:** `npm test`, `npm run lint`, `npm run build`.
- **Mutation checks** for each new guard:
  - float arithmetic, overage rounded down, or tax rounded the wrong way;
  - an issued invoice or its line changed;
  - a second non-void invoice for one tenant and month;
  - a gap or duplicate in invoice numbers;
  - a quantity read outside U2's reader;
  - a card edit, currency change or tax change altering an issued invoice;
  - a currency change missing a card;
  - a mid-month plan change billed in the current month (`tenant_billing_plans` row missing or ignored);
  - the purge deleting invoices;
  - the retention prune deleting invoices younger than the setting;
  - an invoice email sent twice, or to another tenant's admins;
  - relay-ops allowed on any billing route;
  - an unpriced card producing an invoice;
  - incomplete hours not stated.
- Phase-specific tests are in the phase files.

## Acceptance (end of sprint)

Live run on a **reset lab DB with current `main`**. Record the Git SHA and the latest migration, use traffic probes, keep the logs. Use a test clock (or a short `BILLING_SETTLE_DELAY` and a past month) for the month close.

- **Settings:**
  - The default reads United States / USD, no tax, 8 years, email on.
  - Switch to India / INR: every card shows INR and has a new revision, and the console warned that amounts aren't converted.
  - Set tax to 18 % "GST". Each settings change is audited with a reason.
- **Plan cards:**
  - Create two cards: card A prices relay bytes per GB with an included quantity; card B prices active users per user. Duplicate one as a custom card for a tenant.
  - Edit card A mid-month: after regenerate, the draft uses the edited card; an issued invoice is unchanged; the history shows each revision.
- **Mid-month plan change:** move a tenant from card A to card B mid-month. Its invoice for this month bills card A (the month-start plan); its limits changed immediately; next month bills card B.
- **Invoices:**
  - Two tenants with known usage. After the month close, each draft's lines, tax and total match U3's usage report, the cards and the tax setting (amounts checked by hand).
  - An unpriced tenant gets no invoice.
  - A 5-minute controller stop shows as incomplete hours.
  - Regenerate twice: still one invoice.
  - Issue it: the next number is assigned, a SQL `UPDATE` is refused, and the tenant's admins receive the email (HTML and text, correct lines, no console link). Resend sends it again; issuing alone sends it only once.
  - Mark it paid. Void another invoice: the void notice is sent.
  - With email turned off, issue sends nothing.
  - Purge a tenant mid-month: its final invoice is generated with its kept name, recorded as "not delivered", and its invoices survive.
- **Console:**
  - Billing settings, Plans as cards (create, edit, duplicate, history), the Invoices page, the invoice detail with actions and email status, the tenant's Invoices section, CSV/JSON export and the print view.
  - Relay-ops get 403 on the API and see no billing pages.
- **X3:** each follow-up from the Sprint 25 acceptance run checked live.

Tracker: F34 ticked only after its live check.

## Checklist

- [ ] Sprint 25 stack merged (#244 → #248); Sprint 25 acceptance run (both) done; follow-ups recorded as X3; tracker F32, F33 ticked
- [x] Phase 0: Q1–Q6 answered 2026-11-23; D-62 … D-70 written into the Decision Record (amendment 2026-11-23, billing)
- [x] Documents signed off 2026-11-23 (by Sathiya, who instructed that this override the product-owner sign-off step; Barath reviews in PR #249)
- [ ] X3: Sprint 25 follow-ups (Sathiya)
- [ ] P1a: Billing schema `073_plan_cards`, `074_invoices` (Barath)
- [ ] P1b: Plan card + billing settings API (Barath)
- [ ] P2: Invoice generator and lifecycle (Barath)
- [ ] P3: Invoice API + tenant email (Sathiya)
- [ ] P4: Billing console (Sathiya)
- [ ] Sprint acceptance run (both); tracker F34

## Post-Sprint Fixes

_None yet._

## Out of scope

- Payment collection (Stripe or any processor), stored payment methods, dunning, and automatic suspension or any enforcement.
- Tenant-facing billing pages in the admin UI (tenants get the email only).
- Tax IDs, per-country or per-tenant tax rules, and more than one currency at a time; currency conversion of amounts.
- Proration; discounts, coupons, credits, trials, annual plans; refunds and credit notes.
- Server-side PDF rendering (the console prints its own view; the email carries the invoice in HTML).
- Direct-path traffic metering.
- F19 (incl. a `billing` role), F20, F22, F28/F29, F31, F35, F37, F39, F40.
