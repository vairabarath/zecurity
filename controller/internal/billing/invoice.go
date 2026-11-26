package billing

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Invoice rows (074_). An invoice is a snapshot; once issued the DB refuses
// every change but -> paid / -> void (D-66).

// Invoice statuses.
const (
	StatusDraft  = "draft"
	StatusIssued = "issued"
	StatusPaid   = "paid"
	StatusVoid   = "void"
)

// ErrInvoiceNotFound: no such invoice.
var ErrInvoiceNotFound = errors.New("invoice not found")

// Invoice is one invoice with its lines.
type Invoice struct {
	ID               string
	Number           *string
	TenantID         string
	TenantName       string
	TenantSlug       string
	Month            time.Time
	Status           string
	Currency         string
	PlanID           string
	PlanName         string
	PlanRevision     int
	SubtotalMinor    int64
	TaxRateBP        int
	TaxLabel         string
	TaxMinor         int64
	TotalMinor       int64
	CollectedMinutes int
	MonthMinutes     int
	IncompleteHours  int
	GeneratedAt      time.Time
	IssuedAt         *time.Time
	IssuedBy         *string
	PaidAt           *time.Time
	PaidReference    *string
	PaidBy           *string
	VoidedAt         *time.Time
	VoidedBy         *string
	VoidReason       *string
	Lines            []Line
}

// AuditView is the invoice as written into audit details (before/after):
// status, number and amounts, never tenant contacts.
func (i Invoice) AuditView() map[string]any {
	v := map[string]any{
		"status":         i.Status,
		"tenant_id":      i.TenantID,
		"month":          i.Month.Format("2006-01-02"),
		"currency":       i.Currency,
		"plan_id":        i.PlanID,
		"plan_revision":  i.PlanRevision,
		"subtotal_minor": i.SubtotalMinor,
		"tax_rate_bp":    i.TaxRateBP,
		"tax_minor":      i.TaxMinor,
		"total_minor":    i.TotalMinor,
	}
	if i.Number != nil {
		v["number"] = *i.Number
	}
	if i.PaidAt != nil {
		v["paid_at"] = i.PaidAt.Format("2006-01-02")
	}
	if i.PaidReference != nil {
		v["paid_reference"] = *i.PaidReference
	}
	if i.VoidReason != nil {
		v["void_reason"] = *i.VoidReason
	}
	return v
}

const invoiceColumns = `id::text, number, tenant_id::text, tenant_name, tenant_slug, month, status, currency,
       plan_id::text, plan_name, plan_revision, subtotal_minor, tax_rate_bp, tax_label, tax_minor, total_minor,
       collected_minutes, month_minutes, incomplete_hours, generated_at, issued_at, issued_by,
       paid_at, paid_reference, paid_by, voided_at, voided_by, void_reason`

func scanInvoice(row pgx.Row) (Invoice, error) {
	var i Invoice
	err := row.Scan(&i.ID, &i.Number, &i.TenantID, &i.TenantName, &i.TenantSlug, &i.Month, &i.Status, &i.Currency,
		&i.PlanID, &i.PlanName, &i.PlanRevision, &i.SubtotalMinor, &i.TaxRateBP, &i.TaxLabel, &i.TaxMinor, &i.TotalMinor,
		&i.CollectedMinutes, &i.MonthMinutes, &i.IncompleteHours, &i.GeneratedAt, &i.IssuedAt, &i.IssuedBy,
		&i.PaidAt, &i.PaidReference, &i.PaidBy, &i.VoidedAt, &i.VoidedBy, &i.VoidReason)
	if errors.Is(err, pgx.ErrNoRows) {
		return Invoice{}, ErrInvoiceNotFound
	}
	if err != nil {
		return Invoice{}, fmt.Errorf("read invoice: %w", err)
	}
	i.Month = time.Date(i.Month.Year(), i.Month.Month(), 1, 0, 0, 0, 0, time.UTC)
	return i, nil
}

func loadLines(ctx context.Context, q Querier, id string) ([]Line, error) {
	rows, err := q.Query(ctx, `
SELECT position, kind, coalesce(meter, ''), quantity, included, unit, billable_units, unit_price_minor, amount_minor
  FROM invoice_lines WHERE invoice_id = $1 ORDER BY position`, id)
	if err != nil {
		return nil, fmt.Errorf("read invoice lines: %w", err)
	}
	defer rows.Close()
	var out []Line
	for rows.Next() {
		var l Line
		if err := rows.Scan(&l.Position, &l.Kind, &l.Meter, &l.Quantity, &l.Included, &l.Unit, &l.BillableUnits, &l.UnitPriceMinor, &l.AmountMinor); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// loadInvoice reads an invoice (and its lines when withLines).
func loadInvoice(ctx context.Context, q Querier, id string, withLines bool) (Invoice, error) {
	i, err := scanInvoice(q.QueryRow(ctx, `SELECT `+invoiceColumns+` FROM invoices WHERE id = $1`, id))
	if err != nil {
		return Invoice{}, err
	}
	if withLines {
		if i.Lines, err = loadLines(ctx, q, id); err != nil {
			return Invoice{}, err
		}
	}
	return i, nil
}

// GetInvoice reads one invoice with its lines.
func GetInvoice(ctx context.Context, q Querier, id string) (Invoice, error) {
	return loadInvoice(ctx, q, id, true)
}

// lockInvoice reads an invoice FOR UPDATE.
func lockInvoice(ctx context.Context, tx pgx.Tx, id string) (Invoice, error) {
	return scanInvoice(tx.QueryRow(ctx, `SELECT `+invoiceColumns+` FROM invoices WHERE id = $1 FOR UPDATE`, id))
}

// lockCurrentInvoice reads the tenant's non-void invoice of the month FOR
// UPDATE (there is at most one, by the unique index).
func lockCurrentInvoice(ctx context.Context, tx pgx.Tx, tenantID string, month time.Time) (Invoice, error) {
	return scanInvoice(tx.QueryRow(ctx, `SELECT `+invoiceColumns+` FROM invoices
		WHERE tenant_id = $1 AND month = $2 AND status <> 'void' FOR UPDATE`, tenantID, month))
}

func insertLines(ctx context.Context, tx pgx.Tx, id string, lines []Line) error {
	for _, l := range lines {
		var meter *string
		if l.Kind == "meter" {
			m := l.Meter
			meter = &m
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO invoice_lines (invoice_id, position, kind, meter, quantity, included, unit, billable_units, unit_price_minor, amount_minor)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
			id, l.Position, l.Kind, meter, l.Quantity, l.Included, l.Unit, l.BillableUnits, l.UnitPriceMinor, l.AmountMinor); err != nil {
			return fmt.Errorf("insert invoice line: %w", err)
		}
	}
	return nil
}

// insertDraft inserts a draft and its lines. ok=false when a non-void
// invoice for the tenant and month already exists (the unique index; a
// concurrent run won).
func insertDraftInvoice(ctx context.Context, tx pgx.Tx, inv Invoice) (string, bool, error) {
	var id string
	err := tx.QueryRow(ctx, `
INSERT INTO invoices (tenant_id, tenant_name, tenant_slug, month, currency, plan_id, plan_name, plan_revision,
                      subtotal_minor, tax_rate_bp, tax_label, tax_minor, total_minor,
                      collected_minutes, month_minutes, incomplete_hours)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
ON CONFLICT (tenant_id, month) WHERE status <> 'void' DO NOTHING
RETURNING id::text`,
		inv.TenantID, inv.TenantName, inv.TenantSlug, inv.Month, inv.Currency, inv.PlanID, inv.PlanName, inv.PlanRevision,
		inv.SubtotalMinor, inv.TaxRateBP, inv.TaxLabel, inv.TaxMinor, inv.TotalMinor,
		inv.CollectedMinutes, inv.MonthMinutes, inv.IncompleteHours).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("insert draft: %w", err)
	}
	return id, true, insertLines(ctx, tx, id, inv.Lines)
}

// rewriteDraft replaces a draft's figures and lines, keeping its id.
func rewriteDraft(ctx context.Context, tx pgx.Tx, inv Invoice) error {
	if _, err := tx.Exec(ctx, `
UPDATE invoices
   SET tenant_name = $2, tenant_slug = $3, currency = $4, plan_id = $5, plan_name = $6, plan_revision = $7,
       subtotal_minor = $8, tax_rate_bp = $9, tax_label = $10, tax_minor = $11, total_minor = $12,
       collected_minutes = $13, month_minutes = $14, incomplete_hours = $15, generated_at = now()
 WHERE id = $1 AND status = 'draft'`,
		inv.ID, inv.TenantName, inv.TenantSlug, inv.Currency, inv.PlanID, inv.PlanName, inv.PlanRevision,
		inv.SubtotalMinor, inv.TaxRateBP, inv.TaxLabel, inv.TaxMinor, inv.TotalMinor,
		inv.CollectedMinutes, inv.MonthMinutes, inv.IncompleteHours); err != nil {
		return fmt.Errorf("rewrite draft: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM invoice_lines WHERE invoice_id = $1`, inv.ID); err != nil {
		return fmt.Errorf("clear draft lines: %w", err)
	}
	return insertLines(ctx, tx, inv.ID, inv.Lines)
}
