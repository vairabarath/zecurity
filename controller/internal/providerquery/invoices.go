package providerquery

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Invoice reads (Sprint 26 P3, F34; D-66, D-68, D-70). Billing is tenant data:
// the handlers allow super-admins only (invoice.read) and audit the reads.
// Email deliveries are shown as kind, status, time and recipient count,
// never addresses (none are stored). This package doesn't import
// internal/billing (billing imports it for U2's reader).

// InvoiceRow is an invoice as the list and detail return it.
type InvoiceRow struct {
	ID            string     `json:"id"`
	Number        *string    `json:"number"`
	TenantID      string     `json:"tenant_id"`
	TenantName    string     `json:"tenant_name"`
	TenantSlug    string     `json:"tenant_slug"`
	Purged        bool       `json:"purged"`
	Month         string     `json:"month"` // YYYY-MM-DD, first day of the UTC month
	Status        string     `json:"status"`
	Currency      string     `json:"currency"`
	PlanName      string     `json:"plan_name"`
	PlanRevision  int        `json:"plan_revision"`
	SubtotalMinor int64      `json:"subtotal_minor"`
	TaxRateBP     int        `json:"tax_rate_bp"`
	TaxLabel      string     `json:"tax_label"`
	TaxMinor      int64      `json:"tax_minor"`
	TotalMinor    int64      `json:"total_minor"`
	Collected     int        `json:"collected_minutes"`
	MonthMinutes  int        `json:"month_minutes"`
	Incomplete    int        `json:"incomplete_hours"`
	GeneratedAt   time.Time  `json:"generated_at"`
	IssuedAt      *time.Time `json:"issued_at"`
	IssuedBy      *string    `json:"-"`
	PaidAt        *string    `json:"paid_at"` // YYYY-MM-DD
	PaidReference *string    `json:"paid_reference"`
	PaidBy        *string    `json:"-"`
	VoidedAt      *time.Time `json:"voided_at"`
	VoidedBy      *string    `json:"-"`
	VoidReason    *string    `json:"void_reason"`
}

// InvoiceListItem is one row of GET /provider/invoices.
type InvoiceListItem struct {
	InvoiceRow
	LastEmailStatus *string `json:"last_email_status"`
}

// InvoiceStatusTotal sums one status of a month.
type InvoiceStatusTotal struct {
	Count         int64 `json:"count"`
	SubtotalMinor int64 `json:"subtotal_minor"`
	TaxMinor      int64 `json:"tax_minor"`
	TotalMinor    int64 `json:"total_minor"`
}

// InvoicePage is GET /provider/invoices: a keyset page plus the totals per
// status of the filtered month (the status filter doesn't narrow the totals).
type InvoicePage struct {
	Items      []InvoiceListItem             `json:"items"`
	Totals     map[string]InvoiceStatusTotal `json:"totals"`
	Currency   string                        `json:"currency"`
	NextCursor *string                       `json:"next_cursor"`
}

// InvoiceFilter filters the list. Month zero = all months.
type InvoiceFilter struct {
	Month    time.Time
	Status   string
	TenantID string
	Cursor   string
	Limit    int
}

// InvoiceLineRow is one invoice line (base: the meter fields are null).
type InvoiceLineRow struct {
	Position       int     `json:"position"`
	Kind           string  `json:"kind"`
	Meter          *string `json:"meter"`
	Quantity       *int64  `json:"quantity"`
	Included       *int64  `json:"included"`
	Unit           *int64  `json:"unit"`
	BillableUnits  *int64  `json:"billable_units"`
	UnitPriceMinor *int64  `json:"unit_price_minor"`
	AmountMinor    int64   `json:"amount_minor"`
}

// InvoiceEmailRow is one delivery (never addresses).
type InvoiceEmailRow struct {
	Kind            string     `json:"kind"`
	Seq             int        `json:"seq"`
	Status          string     `json:"status"`
	RecipientsCount int        `json:"recipients_count"`
	Attempts        int        `json:"attempts"`
	SentAt          *time.Time `json:"sent_at"`
	LastError       *string    `json:"last_error"`
	CreatedAt       time.Time  `json:"created_at"`
}

// InvoiceStatusEvent is one status change, from the invoice's own fields.
type InvoiceStatusEvent struct {
	Status string    `json:"status"`
	At     time.Time `json:"at"`
	Actor  string    `json:"actor"`
	Reason *string   `json:"reason"`
}

// InvoiceDetail is GET /provider/invoices/{id}.
type InvoiceDetail struct {
	InvoiceRow
	Lines        []InvoiceLineRow     `json:"lines"`
	Emails       []InvoiceEmailRow    `json:"emails"`
	History      []InvoiceStatusEvent `json:"history"`
	EmailTenants bool                 `json:"email_tenants"`
	ProviderName string               `json:"provider_name"`
}

var invoiceStatuses = map[string]bool{"draft": true, "issued": true, "paid": true, "void": true}

const invoiceRowSelect = `
SELECT i.id::text, i.number, i.tenant_id::text, i.tenant_name, i.tenant_slug,
       NOT EXISTS (SELECT 1 FROM workspaces w WHERE w.id = i.tenant_id),
       to_char(i.month, 'YYYY-MM-DD'), i.status, i.currency, i.plan_name, i.plan_revision,
       i.subtotal_minor, i.tax_rate_bp, i.tax_label, i.tax_minor, i.total_minor,
       i.collected_minutes, i.month_minutes, i.incomplete_hours, i.generated_at, i.issued_at, i.issued_by,
       to_char(i.paid_at, 'YYYY-MM-DD'), i.paid_reference, i.paid_by, i.voided_at, i.voided_by, i.void_reason`

func scanInvoiceRow(row pgx.Row, extra ...any) (InvoiceRow, error) {
	var r InvoiceRow
	dst := []any{&r.ID, &r.Number, &r.TenantID, &r.TenantName, &r.TenantSlug, &r.Purged, &r.Month, &r.Status, &r.Currency,
		&r.PlanName, &r.PlanRevision, &r.SubtotalMinor, &r.TaxRateBP, &r.TaxLabel, &r.TaxMinor, &r.TotalMinor,
		&r.Collected, &r.MonthMinutes, &r.Incomplete, &r.GeneratedAt, &r.IssuedAt, &r.IssuedBy,
		&r.PaidAt, &r.PaidReference, &r.PaidBy, &r.VoidedAt, &r.VoidedBy, &r.VoidReason}
	if err := row.Scan(append(dst, extra...)...); err != nil {
		return InvoiceRow{}, err
	}
	r.GeneratedAt = r.GeneratedAt.UTC()
	for _, t := range []**time.Time{&r.IssuedAt, &r.VoidedAt} {
		if *t != nil {
			u := (*t).UTC()
			*t = &u
		}
	}
	return r, nil
}

// ListInvoices returns a keyset page (month newest first, then id) and the
// filtered month's totals per status, in the settings currency.
func ListInvoices(ctx context.Context, q Querier, f InvoiceFilter) (InvoicePage, error) {
	if f.Status != "" && !invoiceStatuses[f.Status] {
		return InvoicePage{}, ErrInvalidFilter
	}
	if f.TenantID != "" && !IsUUID(f.TenantID) {
		return InvoicePage{}, ErrInvalidFilter
	}
	cur, err := DecodeCursor(f.Cursor)
	if err != nil {
		return InvoicePage{}, err
	}
	limit := ClampLimit(f.Limit)
	var month, tenant, status any
	if !f.Month.IsZero() {
		month = f.Month
	}
	if f.TenantID != "" {
		tenant = f.TenantID
	}
	if f.Status != "" {
		status = f.Status
	}
	cm, cid := keysetArgs(cur)
	rows, err := q.Query(ctx, invoiceRowSelect+`,
       (SELECT e.status FROM invoice_emails e WHERE e.invoice_id = i.id ORDER BY e.created_at DESC, e.seq DESC LIMIT 1)
  FROM invoices i
 WHERE ($1::date IS NULL OR i.month = $1::date)
   AND ($2::uuid IS NULL OR i.tenant_id = $2::uuid)
   AND ($3::text IS NULL OR i.status = $3::text)
   AND ($4::timestamptz IS NULL OR (i.month, i.id) < (($4::timestamptz AT TIME ZONE 'UTC')::date, $5::uuid))
 ORDER BY i.month DESC, i.id DESC
 LIMIT $6`, month, tenant, status, cm, cid, limit+1)
	if err != nil {
		return InvoicePage{}, fmt.Errorf("list invoices: %w", err)
	}
	page := InvoicePage{Items: []InvoiceListItem{}, Totals: map[string]InvoiceStatusTotal{}}
	for rows.Next() {
		var it InvoiceListItem
		r, err := scanInvoiceRow(rows, &it.LastEmailStatus)
		if err != nil {
			rows.Close()
			return InvoicePage{}, fmt.Errorf("scan invoice: %w", err)
		}
		it.InvoiceRow = r
		page.Items = append(page.Items, it)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return InvoicePage{}, fmt.Errorf("list invoices: %w", err)
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		last := page.Items[limit-1]
		m, _ := time.Parse("2006-01-02", last.Month)
		next := EncodeCursor(m, last.ID)
		page.NextCursor = &next
	}
	trows, err := q.Query(ctx, `
SELECT status, count(*), coalesce(sum(subtotal_minor), 0)::bigint, coalesce(sum(tax_minor), 0)::bigint, coalesce(sum(total_minor), 0)::bigint
  FROM invoices
 WHERE ($1::date IS NULL OR month = $1::date) AND ($2::uuid IS NULL OR tenant_id = $2::uuid)
 GROUP BY status`, month, tenant)
	if err != nil {
		return InvoicePage{}, fmt.Errorf("invoice totals: %w", err)
	}
	for trows.Next() {
		var s string
		var t InvoiceStatusTotal
		if err := trows.Scan(&s, &t.Count, &t.SubtotalMinor, &t.TaxMinor, &t.TotalMinor); err != nil {
			trows.Close()
			return InvoicePage{}, err
		}
		page.Totals[s] = t
	}
	trows.Close()
	if err := trows.Err(); err != nil {
		return InvoicePage{}, err
	}
	if err := q.QueryRow(ctx, `SELECT currency FROM billing_settings`).Scan(&page.Currency); err != nil {
		return InvoicePage{}, fmt.Errorf("billing currency: %w", err)
	}
	return page, nil
}

// GetInvoice reads one invoice with its lines, email deliveries and status
// history. providerName is the BILLING_PROVIDER_NAME the email uses.
func GetInvoice(ctx context.Context, q Querier, id, providerName string) (InvoiceDetail, error) {
	if !IsUUID(id) {
		return InvoiceDetail{}, ErrInvalidID
	}
	r, err := scanInvoiceRow(q.QueryRow(ctx, invoiceRowSelect+` FROM invoices i WHERE i.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return InvoiceDetail{}, ErrNotFound
	}
	if err != nil {
		return InvoiceDetail{}, fmt.Errorf("get invoice: %w", err)
	}
	d := InvoiceDetail{InvoiceRow: r, Lines: []InvoiceLineRow{}, Emails: []InvoiceEmailRow{}, ProviderName: providerName}
	rows, err := q.Query(ctx, `
SELECT position, kind, meter, quantity, included, unit, billable_units, unit_price_minor, amount_minor
  FROM invoice_lines WHERE invoice_id = $1 ORDER BY position`, id)
	if err != nil {
		return InvoiceDetail{}, fmt.Errorf("invoice lines: %w", err)
	}
	for rows.Next() {
		var l InvoiceLineRow
		if err := rows.Scan(&l.Position, &l.Kind, &l.Meter, &l.Quantity, &l.Included, &l.Unit, &l.BillableUnits, &l.UnitPriceMinor, &l.AmountMinor); err != nil {
			rows.Close()
			return InvoiceDetail{}, err
		}
		d.Lines = append(d.Lines, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return InvoiceDetail{}, err
	}
	rows, err = q.Query(ctx, `
SELECT kind, seq, status, recipients_count, attempts, sent_at, last_error, created_at
  FROM invoice_emails WHERE invoice_id = $1 ORDER BY created_at, kind, seq`, id)
	if err != nil {
		return InvoiceDetail{}, fmt.Errorf("invoice emails: %w", err)
	}
	for rows.Next() {
		var e InvoiceEmailRow
		if err := rows.Scan(&e.Kind, &e.Seq, &e.Status, &e.RecipientsCount, &e.Attempts, &e.SentAt, &e.LastError, &e.CreatedAt); err != nil {
			rows.Close()
			return InvoiceDetail{}, err
		}
		e.CreatedAt = e.CreatedAt.UTC()
		d.Emails = append(d.Emails, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return InvoiceDetail{}, err
	}
	if err := q.QueryRow(ctx, `SELECT email_tenants FROM billing_settings`).Scan(&d.EmailTenants); err != nil {
		return InvoiceDetail{}, fmt.Errorf("email setting: %w", err)
	}
	d.History = invoiceHistory(r)
	return d, nil
}

func deref(s *string, def string) string {
	if s == nil {
		return def
	}
	return *s
}

// invoiceHistory lists the status changes the invoice's own fields record.
func invoiceHistory(r InvoiceRow) []InvoiceStatusEvent {
	h := []InvoiceStatusEvent{{Status: "draft", At: r.GeneratedAt, Actor: "system"}}
	if r.IssuedAt != nil {
		h = append(h, InvoiceStatusEvent{Status: "issued", At: *r.IssuedAt, Actor: deref(r.IssuedBy, "")})
	}
	if r.PaidAt != nil {
		at, _ := time.Parse("2006-01-02", *r.PaidAt)
		h = append(h, InvoiceStatusEvent{Status: "paid", At: at, Actor: deref(r.PaidBy, ""), Reason: r.PaidReference})
	}
	if r.VoidedAt != nil {
		h = append(h, InvoiceStatusEvent{Status: "void", At: *r.VoidedAt, Actor: deref(r.VoidedBy, ""), Reason: r.VoidReason})
	}
	return h
}
