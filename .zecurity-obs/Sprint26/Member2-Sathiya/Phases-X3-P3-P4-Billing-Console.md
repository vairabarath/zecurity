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
  - **Retries:** backoff 1 min doubling, capped at 6 h, at most 12 attempts (about a day), then `failed` (visible in the console; resend is possible). A send error never changes the invoice.
  - **At most once:**
    - a row is claimed `pending → sending` before the send and finished after it;
    - a row left in `sending` by a restart becomes `failed` ("delivery unknown") and is never re-sent automatically;
    - only "every recipient refused" is retried;
    - a partial send, or a send that timed out (it may still go out), is `failed` without retry, and a super-admin can resend.
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

### Implementation record
- **Started 2026-11-23** on `feat/sprint26-p3-invoice-email` (based on the Sprint 25 stack top `feat/sprint25-u4-usage-alerts`; rebase on `main` after the stack and P1a merge). This is the part that doesn't need P1a's tables.
  - `invitation.Emailer.SendMultipart`: multipart/alternative, plain text plus HTML, quoted-printable UTF-8, RFC 2047 subject. It refuses multi-line headers. In dev mode (no `SMTP_HOST`) it logs the subject only.
  - `internal/billing/money.go`: integer money formatting with ISO 4217 exponents (0, 2 or 3 digits), rate as a percentage, meter quantities (GB rounded down, minutes, users) and price units.
  - `internal/billing/email.go`: `ComposeInvoiceEmail(kind, invoice snapshot)`.
    - Content: invoice or void notice (a resend follows the invoice's status); lines; subtotal; tax line or "Tax not included."; total; incomplete-hours note.
    - Safety: `html/template` escaping; tenant and operator text kept to one line and capped; no console link.
  - `internal/billing/emailworker.go`: `EmailWorker` over an `EmailStore` interface (`ClaimDue`, `FailInterrupted`, `LoadInvoice`, `TenantAdmins`, `Finish`) and a `Mailer`. It follows the at-most-once rules above; SMTP errors are stored with addresses replaced by `<address>`.
- **Tests:**
  - `internal/invitation` multipart parse-back and header-injection tests;
  - `internal/billing` unit tests for formatting, content, escaping, void and resend, own-tenant recipients, not delivered, retry with backoff, max attempts, partial and timeout not retried, transient errors, sanitized errors.
- **Mutation checks**, each caught by a test: timeout retried; partial retried; address not sanitized; no-admin case sending; max attempts ignored; void showing the invoice; HTML rendered with `text/template`; unsanitized subject.
- **Gates:** `go build ./...`, `go vet`, `gofmt`; the full `go test ./...` with the DB and Valkey env passed (50 packages ok).
- **Live check (part, 2026-11-23): Pass, 14/14.** Run against a local capture SMTP server plus one real Gmail SMTP send (`[LIVE CHECK]` subject, sender mailbox to itself); recorded in `Planning/Pending-Live-Tests.md` (section "F34 (P3, part)" and the results log). The full suite skipped only the 2 known hard-coded tests.
- **Completed 2026-11-26** on the same branch, after P2 was merged into it (`068f2747`, merge rather than rebase because the branch is pushed); commit `e2b11d60`, local only.
  - `internal/providerquery/invoices.go`: `ListInvoices` (keyset on month and id, last email status, totals per status, settings currency) and `GetInvoice` (lines, deliveries without addresses, status history from the invoice's own fields).
  - `internal/provider/invoice_handlers.go`: the 7 routes with the console contract above.
    - Reads are audited before the body is written: `invoice.fleet.read` once per list request, `invoice.read` per invoice.
    - Writes call `internal/billing`'s lifecycle and commit with their audit row (reason, before/after).
    - Regenerate writes one audit row listing every draft it wrote.
  - `internal/billing/emailstore.go`: `SQLEmailStore`.
    - Claim: `pending → sending` with `SKIP LOCKED`; the claim time goes in `next_attempt_at`.
    - Rows a restart left in `sending` become `failed` and are never re-sent.
    - Recipients are the tenant's own active admins at send time.
  - `main.go`: routes; `BILLING_PROVIDER_NAME` (default "Zecurity"); the email worker (`BILLING_EMAIL_INTERVAL`, default 30 s) on the F14 sender. The API answers even with `BILLING_ENABLED=false`.
  - **Tests:** `internal/provider/invoice_handlers_test.go`:
    - relay-ops 403 on all 7 routes (nothing audited);
    - list, paging, totals and bad input;
    - a purged tenant's snapshot; detail;
    - the 409/400 errors; the issue, resend, paid, void, regenerate flow;
    - no address in any audit row;
    - the SQL store with the worker.
  - **Mutation checks**, each caught: relay-ops allowed; read audit missing; another tenant's admins; suspended admins; a claim keeping `pending` (sent twice); an interrupted row re-sent; an address in an audit row; an audit without `before`.

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
    - an optional price: base fee, and for each meter a "Billed" toggle with included quantity, unit and unit price. **Every meter starts off** in a new card (session minutes are never billed by default; the minutes meters are an editable choice per card).
    - Bytes are entered as GB (unit = 10⁹), minutes as hours or minutes, users as users. Amounts are entered in the major unit and converted to minor units **as integers** (string parsing, no float math).
    - Reason required. Saved in one call.
  - **Edit card:**
    - The same form, prefilled; any field in any combination; one call; reason required.
    - When tenants use it: "N tenants use this card. Changes apply from next month; this month is billed at the card as it was on the 1st. Issued invoices don't change." (D-63, changed 2026-11-24)
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

### Implementation record
- **Foundations started 2026-11-23** on `feat/sprint26-p4-console-foundations` (based on `feat/sprint25-u4-usage-alerts`). These are pure functions and components with tests; **not wired** into routes, navigation, `src/api/billing.ts` or `api-surface.test.ts`, because the endpoints don't exist until P1b / P3. The API calls are passed in as props.
  - **`src/api/types.ts`:** billing shapes from this phase file (`PlanPrice`, `MeterPrice`, `PlanCardSummary`, `BillingSettings`, `Invoice`, `InvoiceLine`, `InvoiceEmailDelivery`, `BILLING_METERS`); types only, no calls.
  - **`src/lib/billing-money.ts`:**
    - typed amounts are parsed to minor units with string arithmetic (`19.99` → 1999; too many decimals refused);
    - money is formatted as "INR 1,991.84" with ISO 4217 exponents; it mirrors the controller's `internal/billing/money.go` (#250), using the same test values, so the console and the email print the same amounts;
    - tax percent ↔ basis points; meter labels, units, quantities.
  - **`src/lib/plan-price-form.ts`:** form text ↔ `PlanPrice`, plus the edit payload rule: unchanged → left out, billing off → `null`, otherwise the new price.
  - **`src/components/PlanCard.tsx`:** a plan card showing the price or "Not billable", "50 GB included, then INR 8.00 per GB", the six limits, the tenant count and revision, and optional Edit / Duplicate / History actions.
  - **`src/components/PlanCardDialog.tsx`:** new, edit and duplicate in one dialog.
    - The existing name, description and limits fields, plus a **Price** section: billing on or off, the monthly fee, and per meter "Bill …" with included quantity, unit and price.
    - Reason required; one save = one call. The edit view says how many tenants use the card and how price changes reach invoices.
    - Scrolls inside the viewport.
  - **`src/components/BillingSettingsForm.tsx`:**
    - country (fills in the currency) and currency; a currency change states that all N cards switch with no conversion, and needs the "I'll review the card prices" acknowledgement;
    - tax "No tax" or a name and rate; retention 1–30 years; the email-to-tenants toggle;
    - reason; sends only the changed fields.
    - Countries come from `src/lib/billing-countries.ts`, a **placeholder** until the P1b settings API serves the list.
  - **`src/lib/invoiceExport.ts`:** invoices CSV, lines CSV and JSON. Amounts are decimal text with a currency column; formula cells are neutralised by reusing the audit export's `csvCell`.
  - **`src/components/InvoicePrint.tsx`** plus an `@media print` block in `index.css`: prints only the invoice, black on white. Drafts carry a DRAFT watermark; a void invoice notes it doesn't need paying; "Tax not included."; the incomplete-hours note; a deleted tenant is marked.
  - `useWrite` moved from `PlanDialogs.tsx` to `src/lib/useWrite.ts` (lint: component files export components only). No behaviour change.
- **UI review** with the `web-design-guidelines` skill (Vercel, pinned snapshot). Fixed:
  - long names now wrap;
  - `overscroll-contain` on the scrolling dialog;
  - explicit text colour on native selects (Windows dark mode);
  - `name` on every control;
  - example placeholders ending in "…";
  - `aria-live` on the fee error;
  - a heading for the printed invoice;
  - `translate="no"` on the invoice number;
  - the issued date via `Intl.DateTimeFormat`.
- **Kept on purpose:**
  - money is formatted by hand, not with `Intl.NumberFormat`: integer minor units, identical to the email;
  - the console's disable-until-valid submit pattern;
  - the unsaved-changes guard belongs with the page wiring.
- `frontend-design` guidance was followed for wording (sentence case, plain verbs, errors that say how to fix them) and restraint: the components match the console's existing look rather than adding a new style.
- **Tests:** billing-money 50, price form 6, PlanCard 4, PlanCardDialog 8, BillingSettingsForm 7, invoice export 4, InvoicePrint 3.
- **Mutation checks**, each caught by a test: float parsing; extra decimals accepted; an unchanged price still sent; billing off not sending `null`; a currency change without acknowledgement; the tax rate rounded to a whole percent; formula cells not neutralised; the draft watermark missing.
- **Gates:** `npm test` (62 files, 818 tests), `npm run lint` (clean), `npm run build` (ok).
- **Still to do after P1b / P3:**
  - `src/api/billing.ts` and the `api-surface.test.ts` entries;
  - the pages (Plans as cards, Billing settings, Invoices, invoice detail with actions and email status, tenant Invoices section), routes and navigation (super-admin only);
  - the unsaved-changes guard;
  - countries from the settings API;
  - the live check below.

- **Part 2 started 2026-11-25** on `feat/sprint26-p4-part2` (based on `feat/sprint26-p4-console-foundations`). The console is wired end to end against the P1b and P3 contract; **it merges only after P1b and P3**, whose endpoints answer it.
  - **`src/api/billing.ts`:** `getBillingSettings`, `updateBillingSettings`, `listPlanCards`, `createPlanCard`, `updatePlanCard`, `listPlanRevisions`, `listInvoices`, `getInvoice`, `issueInvoice`, `markInvoicePaid`, `voidInvoice`, `regenerateInvoices`, `resendInvoice`.
    - All are in `api-surface.test.ts`.
    - Writes added: `PATCH /provider/billing/settings`, `POST /provider/invoices/{id}/issue | mark-paid | void | resend`, `POST /provider/invoices/regenerate`.
    - Reads added: billing settings, invoices list and detail, plan revisions.
  - **Pages and sections:**
    - Plans as a card grid: new, edit, duplicate, history, remove price, make default, delete. Creating and editing wait for the settings currency.
    - **Billing settings** page.
    - **Invoices** page: month (default last month), status filter, totals per status, keyset pages, Regenerate drafts listing the drafts, export.
    - **Invoice detail:** the print view, status history, email deliveries, and the actions per status (Issue; Mark paid with date/reference/reason; Void with typed number; Resend).
    - **Tenant detail → Invoices.**
    - The plan-change dialog says "Limits change now. Billing changes from next month; this month is billed at the <month-start> card as it was on the 1st."
    - Navigation: a super-admin **Billing** group (Invoices, Billing settings).
  - **Contract details the console assumes** (P1b and P3 must answer these; anything open in this phase file is fixed here):
    - **Settings:** `GET /provider/billing/settings` → `BillingSettings` plus optional `countries: [{code, name, currency}]`. The console falls back to its built-in list when `countries` is absent.
    - **Plan create:** `POST /provider/plans` body `{name, description, limits, price, reason}`.
    - **Plan update:** `PATCH /provider/plans/{id}` body: the changed fields plus `reason`. `price: null` removes the price; an absent `price` keeps it.
    - **Plan revisions:** `GET /provider/plans/{id}/revisions` → `{items: [{revision, changed_at, changed_by, reason, card: {name, description, limits, price}}]}`. `changed_by` is the operator email or `system`. The console computes "what changed" from consecutive revisions.
    - **Tenant detail:** gains `billing_plan: {id, name}`, the plan this month is billed at (`PlanForMonth`, D-65). The console names the current plan when it is absent.
    - **Invoice list:** `GET /provider/invoices` → `{items: [Invoice + last_email_status], totals: {draft|issued|paid|void: {count, subtotal_minor, tax_minor, total_minor}}, currency, next_cursor}`.
    - **Invoice detail:** `GET /provider/invoices/{id}` → the `Invoice` plus `lines`, `emails` (no addresses), `history: [{status, at, actor, reason}]`, `email_tenants` (the D-68 setting at read time) and `provider_name`.
    - **Regenerate:** `POST /provider/invoices/regenerate` → `{regenerated: n}`.
    - **Issue:** `POST …/issue` body `{}` or `{reason}`.
    - **Mark paid:** `POST …/mark-paid` body `{paid_at: "YYYY-MM-DD", reference, reason}`.
    - **Error codes shown** (invoices):
      - `invoice_not_draft`, `invoice_not_issued`, `month_open`;
      - `reason_required`, `reason_too_long`, `reference_too_long`;
      - `invalid_paid_at`, `invoice_not_found`.
    - **Error codes shown** (cards and settings):
      - `invalid_price`, `currency_mismatch`, `price_overflow`;
      - `invalid_settings`, `invalid_currency`, `invalid_country`.
    - **Export** reads each invoice's detail for its lines, one audited `invoice.read` each, capped at 500 per export. A `?include=lines` on the list would cut this to the list reads: a P3 option, not needed for the cap.
  - **Tests:** 25 new across the Plans, Invoices, invoice detail, Billing settings, tenant Invoices and plan-change tests.
  - **Mutation checks**, each caught:
    - void without the typed number;
    - Mark paid / Void shown for a draft;
    - Remove price not sending `null`;
    - Regenerate without the month;
    - the month-start card not named;
    - unpriced cards counted in the currency switch;
    - a future paid date accepted;
    - the Deleted badge missing.
  - **Visual check** in the browser with the billing calls answered in the page (no controller endpoints yet):
    - the Invoices list, invoice detail, Plans grid and Billing settings render as designed;
    - one fix: the settings form was double-wrapped in a card.
  - **Gates:** `npm test` (65 files, 843 tests), `npm run lint` (clean), `npm run build` (ok).
  - **Still to do:**
    - ~~the unsaved-changes guard on the settings and card forms~~ **done** (`feat/sprint26-p4-unsaved-guard`, `054c8f17`, local only):
      - `src/lib/useUnsavedChanges.ts`.
      - **Billing settings:** dirty when any setting differs from what was loaded or last saved; the reason alone doesn't count. Leaving the page while dirty shows the browser's "Leave site?" (`beforeunload`). A successful save turns the guard off; a failed save keeps it.
      - In-app links aren't blocked: the console uses `<BrowserRouter>`, not a data router, so `useBlocker` isn't available.
      - **Plan card dialog** (new, edit, duplicate): closing while dirty (Cancel, Escape, overlay, X) shows "Discard unsaved changes?" with Keep editing / Discard. No prompt after a successful save.
      - 8 new tests (851 pass); the mutation check (dirty check always false) was caught by 5 tests; lint and build clean.
    - the live check below, once P1b, P2 and P3 are merged.

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
