---
type: phase
sprint: 26
owner: Sathiya (Person 2, M1)
features: [Sprint 25 follow-ups (F32, F33), F34 billing plans and invoicing]
phases: [X3, P3, P4]
decisions: [D-62, D-66, D-67, D-68, D-30, D-40, D-60, D-53]
status: DRAFT pending sign-off (Q1 answered 2026-11-23: editable plan cards; Q2–Q6 use the recommended answers as working assumptions); no implementation until signed off
---

# Sprint 26 — Sathiya: X3 follow-ups, P3 invoice API, P4 billing console (F34)

Read `Sprint26/path.md` first (order, conflict zones, rules). Binding decisions after sign-off: Decision Record amendment **billing**:
- **D-68** who can read and write; amends D-30;
- **D-66** invoice lifecycle;
- **D-67** purge and retention;
- **D-62** scope: no payment, tax or enforcement.

These build on **D-40/D-60** (tenant data is super-admin only), **D-53** (reason and before/after audit) and **OQ-1** (tenant reads audited).

Order: **X3 → P3 → P4**.
- P3's **reads** start when Barath's **P1a** (`073_`, `074_`) is merged; they're tested against rows the tests insert.
- P3's **writes** call P2's `internal/billing` lifecycle functions, so they wait for P2.
- P4 needs P1b and P3.

---

## X3 — Sprint 25 follow-ups

Filled in after the Sprint 25 acceptance run (both, reset lab DB, current `main`, with the stack #244 → #248 merged). Each finding gets:
- a separate small change;
- its decision, if one is needed;
- a test;
- a live recheck.

Then tick F32 and F33 in the tracker.

---

## P3 — Invoice API — D-68, D-66

### What exists
- P1a tables: `invoices`, `invoice_lines`, `plan_prices`, `plan_card_revisions`. P2's `internal/billing`: `Regenerate`, `Issue`, `MarkPaid`, `Void` (one implementation; don't reimplement the state changes).
- Patterns:
  - `providerquery` services without `net/http` (D-04);
  - keyset paging (`Page[T]`, cursors);
  - U2's tenant-read audit (`tenant.usage.read`) and fleet audit (`usage.fleet.read`);
  - `provider.WithChange` for before/after.

### Build
- `providerquery/invoices.go`:
  - `ListInvoices(ctx, filter{month, status, tenantID}, cursor, limit)`: rows with number, tenant name and slug, `purged` (no live workspace), month, status, currency, subtotal and incomplete hours. Keyset-paged by `(month DESC, number/created, id)`. Also returns **totals per status** for the filtered month (one currency, D-64).
  - `GetInvoice(ctx, id)`: the invoice, its lines in `position` order, and its status history (from the row's `issued_*`, `paid_*` and `voided_*` fields).
  - `TenantInvoices(ctx, tenantID, cursor, limit)`.
- Routes:
  - Reads:
    - `GET /provider/invoices?month=&status=&tenant_id=&cursor=&limit=`
    - `GET /provider/invoices/{id}`
  - Writes:
    - `POST /provider/invoices/{id}/issue` (body `{reason?}`)
    - `POST /provider/invoices/{id}/mark-paid` (body `{paid_at, reference, reason}`)
    - `POST /provider/invoices/{id}/void` (body `{reason}`, required)
    - `POST /provider/invoices/regenerate` (body `{month, tenant_id?, reason}`)
- `decide()` actions **`invoice.read`** and **`invoice.write`**: super-admin only by the existing rule; relay-ops get 403.
- Audit:
  - reading one invoice → **`invoice.read`** (target: tenant and invoice id);
  - the list → one **`invoice.fleet.read`** row per request;
  - writes → `invoice.issue`, `invoice.mark_paid`, `invoice.void`, `invoice.regenerate`, each with the reason and before/after (status, number, subtotal).
  - Read actions end in `.read`, so the incident timeline (T1) leaves them out.
- Errors:
  - `409 invoice_not_draft` / `invoice_not_issued` for a wrong transition;
  - `409 month_open` for issuing the current month;
  - `400` for a missing reason on void or mark-paid, or a `reference` over 200 characters.
- Every response includes `Cache-Control: no-store`. No payment data is accepted beyond the free-text reference.

### Tests (DB-backed)
- List:
  - filter by month, status and tenant;
  - paging without repeats or gaps;
  - totals per status;
  - a purged tenant named from its snapshot and marked `purged`.
- Detail: lines in order; status history.
- Writes:
  - each transition calls P2 and returns the new state;
  - a wrong transition → 409;
  - void without a reason → 400;
  - audit before/after with the reason.
- Authorization: relay-ops 403 on every route. Read audit rows written with the right actions.
- **Mutation checks:**
  - relay-ops allowed;
  - read audit missing;
  - a handler changing status with its own SQL instead of P2's function;
  - a void accepted without a reason.

### Live check (P3)
On the P2 live drafts: list and detail match. Issue, mark paid and void through the API. Relay-ops 403. Audit rows present.

---

## P4 — Billing console

### Build
- **API modules:**
  - `provider-console/src/api/billing.ts`: the plan calls gain the card's `price` (`createPlan`, `updatePlan`, `listPlans`, `getPlan`), plus `listPlanRevisions`, `listInvoices`, `getInvoice`, `issueInvoice`, `markInvoicePaid`, `voidInvoice`, `regenerateInvoices`.
  - Each goes into `api-surface.test.ts` on purpose: the reads in `ALLOWED_READS`, the writes in the D-30 write list (amended by D-68). Plan create and update are already listed; their new `price` field is noted there.
- **Plans page as cards** (super-admin):
  - A grid of **plan cards**, one per plan, showing:
    - the name and the monthly price (or "Not billable");
    - each priced meter's included quantity and the price above it ("50 GB included, then $0.10 / GB");
    - the six limits;
    - the number of tenants on it;
    - Default / Legacy badges.
  - **New card:** a form with:
    - name, description, the six limits (number or Unlimited);
    - an optional price: base fee, and for each meter a "Billed" toggle with included quantity, unit and unit price.
    - Bytes are entered as GB (unit = 10⁹), minutes as hours or minutes, users as users. Amounts are entered in the currency's major unit and converted to minor units **as integers** (string parsing, no float math).
    - Reason required. Saved in one call (plan create with `price`).
  - **Edit card:** the same form, prefilled.
    - Change the price, any included quantity, the limits or the name, in any combination, saved in one call (plan update). Reason required.
    - When tenants are on the card, the form says: "N tenants use this card. The change applies to this month's invoices at month close (and to drafts when regenerated). Issued invoices don't change."
  - **Remove price:** makes the card not billable (confirmation, reason).
  - **History:** each card's revision history (who, when, reason, what changed).
  - **Custom card:** "Duplicate" prefills a new card from an existing one, for a tenant-specific deal; assign it with B1's existing plan assignment.
- **Invoices page** (`/invoices`, super-admin nav only):
  - a month picker and a status filter;
  - totals per status;
  - a table (number or "Draft", tenant, status, subtotal, incomplete hours) with keyset pages.
  - "Regenerate drafts" for the month (reason; confirmation lists the drafts that will change).
- **Invoice detail:**
  - header: tenant, month, plan, currency;
  - lines: quantity, included, billable units, unit price, amount;
  - a warning when `incomplete_hours > 0` ("N hours of this month weren't collected; usage is billed as counted");
  - status history.
  - Actions by status:
    - draft: Issue (confirmation; the number is assigned on issue);
    - issued: Mark paid (date, reference, reason) and Void (reason, typed invoice number).
    - Paid and void invoices are read-only.
- **Tenant detail → Invoices:** the tenant's invoices (number, month, status, subtotal), each linking to the invoice.
- **Purged tenants:** invoices show the kept name with a "Deleted" badge and no link to a tenant page (as in U3).
- **Export:** CSV and JSON of the shown month's invoices (one row per invoice, plus a lines file or nested lines in JSON), client-side like F38/U3: formula cells neutralised, a row cap, and incomplete hours included.
- **Printable invoice view:** a print stylesheet on the detail page (browser print to PDF):
  - provider name from config;
  - tenant snapshot, number, month, lines, subtotal;
  - a "Tax not included" note *(Q6)*;
  - drafts print with a "DRAFT" watermark.
  - No server-side PDF.
- Money formatting: from minor units with the currency's exponent (`Intl.NumberFormat`), never by dividing floats for storage.

### Tests
- Per view:
  - requests (one per open, per filter change; no polling);
  - integer conversion of entered amounts (`12.34` → `1234`, `0.1 + 0.2` cases, too many decimals refused);
  - GB/unit conversion;
  - new card and edit card each send one request with the right `price` (omitted vs `null`);
  - the "N tenants use this card" notice;
  - duplicate prefills every field;
  - actions shown only for the right status;
  - the void typed-number check;
  - the incomplete-hours warning;
  - the purged tenant badge without a link;
  - export content and formula neutralising;
  - the print view has a DRAFT watermark on drafts.
- Relay-ops see no Invoices nav entry, no card editing, no card prices and no tenant Invoices section (and the API refuses them anyway).
- `npm test`, `npm run lint`, `npm run build`.

### Live check (P4)
- Create a card, duplicate it as a custom card, edit its price and an included quantity; the history shows each change. The P2 drafts show on the Invoices page and on tenant detail.
- Issue, mark paid and void from the console. Export opened. Print view checked (draft and issued).
- A purged tenant's invoice is shown with its name and "Deleted". Relay-ops see none of it.

---

## Post-Phase Fixes

_None yet._
