---
type: phase
sprint: 26
owner: Sathiya (Person 2, M1)
features: [Sprint 25 follow-ups (F32, F33), F34 billing plans and invoicing]
phases: [X3, P3, P4]
decisions: [D-62, D-63, D-64, D-65, D-66, D-67, D-68, D-69, D-70, D-30, D-40, D-60, D-53]
status: signed off 2026-11-23 (by Sathiya, overriding the product-owner sign-off step; Barath reviews in PR #249); X3 after the Sprint 25 acceptance run, P3 after P1a
---

# Sprint 26 — Sathiya: X3 follow-ups, P3 invoice API and tenant email, P4 billing console (F34)

Read `Sprint26/path.md` first (order, conflict zones, rules). Binding decisions: Decision Record amendment **2026-11-23, billing**:
- **D-70** who can read and write; amends D-30;
- **D-68** tenant invoice email;
- **D-66** lifecycle;
- **D-63** plan cards;
- **D-64** country and currency;
- **D-65** month-start plan;
- **D-67** retention;
- **D-69** tax.

These build on **D-40/D-60** (super-admin only), **D-53** (reason and before/after audit) and **OQ-1** (tenant reads audited).

Order: **X3 → P3 → P4**.
- P3's **reads** start when Barath's **P1a** (`073_`, `074_`) is merged; they're tested against rows the tests insert.
- P3's **writes** call P2's lifecycle functions.
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

## P3 — Invoice API and tenant invoice email — D-70, D-66, D-68

### What exists
- P1a tables: `invoices`, `invoice_lines`, `invoice_emails`, `billing_settings`.
- P2's `internal/billing`: `Regenerate`, `Issue`, `MarkPaid` and `Void` (they enqueue the email rows; don't reimplement them).
- The F14/A2 SMTP sender and its config; the A2 outbox retry pattern.
- `providerquery` (no `net/http`, D-04), keyset paging, the U2 audit pattern (`tenant.usage.read`, `usage.fleet.read`), `provider.WithChange`.

### Build
- **`providerquery/invoices.go`**:
  - `ListInvoices(filter{month, status, tenantID}, cursor, limit)`: number, tenant name and slug, `purged`, month, status, currency, subtotal, tax, total, incomplete hours, and the last email status. Also returns **totals per status** for the month.
  - `GetInvoice(id)`: the invoice, its lines, its status history and its email deliveries (kind, status, time, recipient count; never addresses).
  - `TenantInvoices(tenantID, cursor, limit)`.
- **Routes:**
  - Reads:
    - `GET /provider/invoices?month=&status=&tenant_id=&cursor=&limit=`
    - `GET /provider/invoices/{id}`
  - Writes (each calls P2):
    - `POST /provider/invoices/{id}/issue` (`{reason?}`)
    - `POST /provider/invoices/{id}/mark-paid` (`{paid_at, reference, reason}`)
    - `POST /provider/invoices/{id}/void` (`{reason}`, required)
    - `POST /provider/invoices/regenerate` (`{month, tenant_id?, reason}`)
    - `POST /provider/invoices/{id}/resend` (`{reason}`): issued, paid or void invoices only; enqueues the next `resend` row.
- `decide()` actions **`invoice.read`** and **`invoice.write`** (super-admin only; relay-ops 403).
- **Audit:**
  - reads: `invoice.read` per invoice; `invoice.fleet.read` per list request;
  - writes: `invoice.issue`, `invoice.mark_paid`, `invoice.void`, `invoice.regenerate` and `invoice.resend`, each with the reason and before/after.
- **Errors:** `409 invoice_not_draft` / `invoice_not_issued` / `month_open`; `400` for a missing reason or a reference over 200 characters. `Cache-Control: no-store` on every response.
- **Tenant email sender (`internal/billing/email.go`, D-68):**
  - A queue worker (process-local, D-02) picks `invoice_emails` rows that are `pending` and due.
  - **Recipients:** the tenant's **active admins** (the workspace admin role; resolved at send time). None (e.g. purged tenant) → `not_delivered`.
  - **Body:** HTML and plain text, from the invoice snapshot only:
    - provider name, invoice number, month, tenant name;
    - the lines (quantity, included, billable units, unit price, amount);
    - subtotal, tax (label and rate) or "Tax not included", total;
    - the incomplete-hours note when it applies;
    - for a void notice, the reason.
    - No console link, no secrets, no other tenant's data.
  - **Retries:** exponential backoff up to 24 h, then `failed` (visible in the console; resend is possible). Each row is sent at most once (status moves in a transaction before and after the send). A send error never changes the invoice.
  - Recipient addresses are never written to the provider audit or the logs (only the count).

### Tests (DB-backed)
- **List and detail:**
  - filters; paging without repeats or gaps; totals per status;
  - a purged tenant named from its snapshot;
  - lines in order; email deliveries shown without addresses.
- **Writes:**
  - each transition through P2; a wrong transition → 409; void without a reason → 400;
  - audit before/after with the reason;
  - resend adds a new row each time; issue alone sends once.
- **Email:**
  - recipients are only that tenant's active admins (not other tenants, not disabled users);
  - the body matches the invoice (amounts and currency formatting from minor units);
  - "Tax not included" when the rate is 0;
  - a retry after an SMTP error, and no second send after success;
  - nothing enqueued when `email_tenants` is off;
  - a purged tenant → `not_delivered`.
- Relay-ops 403 on every route.
- **Mutation checks:**
  - relay-ops allowed;
  - read audit missing;
  - a handler changing status with its own SQL;
  - an email sent to another tenant's admins;
  - an email sent twice;
  - recipient addresses in the audit.

### Live check (P3)
- On the P2 drafts: list and detail match. Issue → the tenant's admin mailbox gets the invoice (HTML and text). Resend → one more email. Void → the void notice. Mark paid.
- Email off → nothing sent.
- Relay-ops 403. Audit rows present (with no addresses).

---

## P4 — Billing console

### Build
- **API modules:** `provider-console/src/api/billing.ts`:
  - `getBillingSettings`, `updateBillingSettings`;
  - the plan calls gain the card's `price` (`createPlan`, `updatePlan`, `listPlans`, `getPlan`), plus `listPlanRevisions`;
  - `listInvoices`, `getInvoice`, `issueInvoice`, `markInvoicePaid`, `voidInvoice`, `regenerateInvoices`, `resendInvoice`.
  - All go into `api-surface.test.ts` on purpose (reads in `ALLOWED_READS`, writes in the D-30 list amended by D-70).
- **Billing settings** (super-admin nav, under Plans):
  - **Country** (select) and **currency** (select, filled in from the country, can be changed).
    - On a currency change, a confirmation says: "All N plan cards switch to INR. Amounts are not converted; review each card's prices."
  - **Tax:** "No tax" (default) or a rate (%) and a label (e.g. GST). The rate is entered as a percentage with up to 2 decimals and stored as basis points, converted as integers.
  - **Invoice retention** (years, 1–30, default 8).
  - **Email invoices to tenants** (toggle, default on).
  - Reason required to save.
- **Plans page as cards** (super-admin):
  - **Grid:** one card per plan, showing:
    - the name and the monthly price in the settings currency (or "Not billable");
    - each priced meter ("50 GB included, then ₹8 / GB");
    - the six limits;
    - the number of tenants;
    - Default / Legacy badges.
  - **New card:**
    - name, description, the six limits (number or Unlimited);
    - an optional price: base fee, and for each meter a "Billed" toggle with included quantity, unit and unit price.
    - Bytes are entered as GB (unit = 10⁹), minutes as hours or minutes, users as users. Amounts are entered in the major unit and converted to minor units **as integers** (string parsing, no float math).
    - Reason required. Saved in one call.
  - **Edit card:**
    - The same form, prefilled; any field in any combination; one call; reason required.
    - When tenants use it: "N tenants use this card. Changes apply to this month's invoices at month close (drafts when regenerated). Issued invoices don't change."
  - **Remove price** (confirmation, reason), **History** (revisions: who, when, reason, what changed), **Duplicate** (for a custom card; assign it with B1's plan assignment).
  - **Plan assignment dialog** (B1, extended text only): "Limits change now. Billing changes from next month; this month is billed at <month-start card>."
- **Invoices page** (`/invoices`, super-admin nav):
  - a month picker and a status filter;
  - totals per status (subtotal, tax, total);
  - a table (number or "Draft", tenant, status, total, incomplete hours, email status) with keyset pages;
  - "Regenerate drafts" (reason; the confirmation lists the drafts that will change).
- **Invoice detail:**
  - header: tenant, month, plan and card revision, currency;
  - lines;
  - subtotal, tax (label and rate) or "Tax not included", total;
  - the incomplete-hours warning;
  - status history;
  - **email deliveries** (kind, status, time, recipient count).
  - **Actions:**
    - draft: Issue (the confirmation says the tenant's admins will be emailed when email is on);
    - issued: Mark paid (date, reference, reason), Void (reason, typed invoice number), Resend email (reason);
    - paid / void: Resend email.
- **Tenant detail → Invoices:** the tenant's invoices, linking to each. **Purged tenants** show the kept name with a "Deleted" badge and no tenant link.
- **Export:** CSV and JSON of the month's invoices (lines nested in JSON, a separate lines CSV), client-side like F38/U3: formula cells neutralised, a row cap, incomplete hours, tax and total included.
- **Printable invoice view:**
  - a print stylesheet (browser print to PDF);
  - provider name, tenant snapshot, number, month, lines, subtotal, tax or "Tax not included", total;
  - "DRAFT" watermark on drafts.
- **Money formatting:** `Intl.NumberFormat` with the invoice's currency, from minor units.

### Tests
- **Settings:**
  - choosing India fills in INR;
  - the currency-change confirmation shows the card count;
  - tax percentage → basis points (`18` → `1800`, `7.25` → `725`, 3 decimals refused);
  - retention bounds.
- **Cards:**
  - new and edit each send one request with the right `price` (omitted vs `null`);
  - integer conversion (`12.34` → `1234`; float traps);
  - GB conversion;
  - the "N tenants use this card" notice;
  - duplicate prefills every field;
  - the assignment dialog text names the month-start card.
- **Invoices:**
  - one request per open or filter change (no polling);
  - actions only for the right status;
  - the void typed-number check;
  - the incomplete-hours warning;
  - "Tax not included";
  - email status shown;
  - the purged badge without a link;
  - export content and formula neutralising;
  - the print view's DRAFT watermark.
- Relay-ops see no Billing settings, card prices or editing, Invoices page or tenant Invoices section (and the API refuses them anyway).
- `npm test`, `npm run lint`, `npm run build`.

### Live check (P4)
- In the console:
  - switch the country to India: the cards show INR and the warning appeared;
  - set GST 18 %;
  - create a card, duplicate it as a custom card, and edit a price and an included quantity; the history shows each change;
  - move a tenant mid-month: the dialog names the month-start card.
- **Invoices:** the P2 drafts show with tax and total. Issue → the email arrives. Resend, mark paid and void. Export opened. Print view checked.
- A purged tenant shows "Deleted". Relay-ops see none of it.

---

## Post-Phase Fixes

_None yet._
