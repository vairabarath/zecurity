package billing

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// SQLEmailStore is the EmailStore over invoice_emails (074_, D-68). A claimed
// row's next_attempt_at is set to the claim time, so FailInterrupted can tell
// a row a restart left in 'sending' (claimed before the cutoff).
type SQLEmailStore struct {
	DB *pgxpool.Pool
}

var _ EmailStore = (*SQLEmailStore)(nil)

// ClaimDue moves up to limit due pending rows to sending (oldest first) in
// one statement; SKIP LOCKED keeps two claims from taking the same row.
func (s *SQLEmailStore) ClaimDue(ctx context.Context, now time.Time, limit int) ([]EmailJob, error) {
	rows, err := s.DB.Query(ctx, `
UPDATE invoice_emails e SET status = 'sending', next_attempt_at = $1
  FROM (SELECT invoice_id, kind, seq FROM invoice_emails
         WHERE status = 'pending' AND next_attempt_at <= $1
         ORDER BY created_at, invoice_id, kind, seq
         LIMIT $2 FOR UPDATE SKIP LOCKED) due
 WHERE e.invoice_id = due.invoice_id AND e.kind = due.kind AND e.seq = due.seq
RETURNING e.invoice_id::text, e.kind, e.seq, e.attempts, e.created_at`, now, limit)
	if err != nil {
		return nil, fmt.Errorf("claim invoice emails: %w", err)
	}
	defer rows.Close()
	type claimed struct {
		job EmailJob
		at  time.Time
	}
	var cs []claimed
	for rows.Next() {
		var c claimed
		if err := rows.Scan(&c.job.InvoiceID, &c.job.Kind, &c.job.Seq, &c.job.Attempts, &c.at); err != nil {
			return nil, err
		}
		cs = append(cs, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// RETURNING has no order: oldest first.
	for i := 1; i < len(cs); i++ {
		for j := i; j > 0 && cs[j].at.Before(cs[j-1].at); j-- {
			cs[j], cs[j-1] = cs[j-1], cs[j]
		}
	}
	out := make([]EmailJob, len(cs))
	for i, c := range cs {
		out[i] = c.job
	}
	return out, nil
}

// FailInterrupted marks rows claimed before cutoff and still sending as
// failed: delivery unknown, never re-sent automatically (D-68).
func (s *SQLEmailStore) FailInterrupted(ctx context.Context, cutoff time.Time) (int, error) {
	tag, err := s.DB.Exec(ctx, `
UPDATE invoice_emails SET status = 'failed', last_error = 'interrupted by a controller restart; delivery unknown (resend if needed)'
 WHERE status = 'sending' AND next_attempt_at < $1`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("fail interrupted invoice emails: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// LoadInvoice reads the issued invoice's snapshot for the email.
func (s *SQLEmailStore) LoadInvoice(ctx context.Context, invoiceID string) (EmailInvoice, error) {
	inv, err := GetInvoice(ctx, s.DB, invoiceID)
	if err != nil {
		return EmailInvoice{}, err
	}
	e := EmailInvoice{
		ID: inv.ID, Status: inv.Status, TenantID: inv.TenantID, TenantName: inv.TenantName, Month: inv.Month,
		Currency: inv.Currency, PlanName: inv.PlanName, SubtotalMinor: inv.SubtotalMinor, TaxRateBP: inv.TaxRateBP,
		TaxLabel: inv.TaxLabel, TaxMinor: inv.TaxMinor, TotalMinor: inv.TotalMinor, IncompleteHours: inv.IncompleteHours,
	}
	if inv.Number != nil {
		e.Number = *inv.Number
	}
	if inv.VoidReason != nil {
		e.VoidReason = *inv.VoidReason
	}
	v := func(p *int64) int64 {
		if p == nil {
			return 0
		}
		return *p
	}
	for _, l := range inv.Lines {
		e.Lines = append(e.Lines, EmailLine{Kind: l.Kind, Meter: l.Meter, Quantity: v(l.Quantity), Included: v(l.Included),
			Unit: v(l.Unit), BillableUnits: v(l.BillableUnits), UnitPriceMinor: v(l.UnitPriceMinor), AmountMinor: l.AmountMinor})
	}
	return e, nil
}

// TenantAdmins returns the tenant's own active admins, resolved at send time
// (the rbac guard's definition). A purged tenant has none.
func (s *SQLEmailStore) TenantAdmins(ctx context.Context, tenantID string) ([]string, error) {
	rows, err := s.DB.Query(ctx, `
SELECT email FROM users WHERE tenant_id = $1 AND role = 'admin' AND status = 'active' ORDER BY email`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("tenant admins: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ErrEmailNotClaimed: Finish found no claimed row (already finished).
var ErrEmailNotClaimed = errors.New("invoice email row not in sending")

// Finish records an attempt's outcome on the claimed row.
func (s *SQLEmailStore) Finish(ctx context.Context, job EmailJob, out EmailOutcome) error {
	var lastErr *string
	if out.LastError != "" {
		e := out.LastError
		lastErr = &e
	}
	tag, err := s.DB.Exec(ctx, `
UPDATE invoice_emails
   SET status = $4, attempts = $5, recipients_count = $6,
       next_attempt_at = coalesce($7, next_attempt_at), last_error = $8, sent_at = $9
 WHERE invoice_id = $1 AND kind = $2 AND seq = $3 AND status = 'sending'`,
		job.InvoiceID, job.Kind, job.Seq, out.Status, out.Attempts, out.Recipients, out.NextAttemptAt, lastErr, out.SentAt)
	if err != nil {
		return fmt.Errorf("finish invoice email: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrEmailNotClaimed
	}
	return nil
}
