package billing

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// The invoice lifecycle (D-66), the ONLY implementation: draft → issued →
// paid | void. P3's handlers call these functions; nothing else changes an
// invoice's status. Each runs in the caller's transaction (so the caller's
// audit row commits with it) and returns the invoice before and after.

var (
	// ErrNotDraft: the invoice is not a draft (issue).
	ErrNotDraft = errors.New("invoice is not a draft")
	// ErrNotIssued: the invoice is not issued (mark paid, void).
	ErrNotIssued = errors.New("invoice is not issued")
	// ErrNotSent: a draft has nothing to resend.
	ErrNotSent = errors.New("invoice has not been issued")
	// ErrReasonRequired: void needs a reason.
	ErrReasonRequired = errors.New("reason required")
	// ErrReasonTooLong: over 500 characters.
	ErrReasonTooLong = errors.New("reason too long")
	// ErrReferenceTooLong: a paid reference over 200 characters.
	ErrReferenceTooLong = errors.New("reference too long")
	// ErrInvalidPaidAt: a paid date in the future or before the month.
	ErrInvalidPaidAt = errors.New("invalid paid date")
)

const (
	maxReasonRunes    = 500
	maxReferenceRunes = 200
)

// InvoiceNumber formats a gap-free number: INV-<year>-<6 digits>.
func InvoiceNumber(year int, n int64) string {
	return fmt.Sprintf("INV-%04d-%06d", year, n)
}

// nextNumber takes the year's next number from the counter row, locked until
// the transaction ends: a rolled-back issue releases it, so numbers have no
// gaps and no duplicates (never a SEQUENCE, D-66).
func nextNumber(ctx context.Context, tx pgx.Tx, year int) (string, error) {
	var n int64
	if err := tx.QueryRow(ctx, `
INSERT INTO invoice_number_counters (year, last) VALUES ($1, 1)
ON CONFLICT (year) DO UPDATE SET last = invoice_number_counters.last + 1
RETURNING last`, year).Scan(&n); err != nil {
		return "", fmt.Errorf("invoice number: %w", err)
	}
	return InvoiceNumber(year, n), nil
}

// emailsOn reads the D-68 setting in the transaction.
func emailsOn(ctx context.Context, tx pgx.Tx) (bool, error) {
	var on bool
	if err := tx.QueryRow(ctx, `SELECT email_tenants FROM billing_settings`).Scan(&on); err != nil {
		return false, fmt.Errorf("read email setting: %w", err)
	}
	return on, nil
}

// enqueueEmail adds the (invoice, kind) delivery row; once only for issued
// and voided (the primary key), the next seq for a resend.
func enqueueEmail(ctx context.Context, tx pgx.Tx, invoiceID, kind string) error {
	if kind == "resend" {
		_, err := tx.Exec(ctx, `
INSERT INTO invoice_emails (invoice_id, kind, seq)
SELECT $1, 'resend', coalesce(max(seq), 0) + 1 FROM invoice_emails WHERE invoice_id = $1 AND kind = 'resend'`, invoiceID)
		return err
	}
	_, err := tx.Exec(ctx, `INSERT INTO invoice_emails (invoice_id, kind, seq) VALUES ($1, $2, 1) ON CONFLICT DO NOTHING`, invoiceID, kind)
	return err
}

// Issue moves a draft to issued with the next number of the issue year and,
// when email_tenants is on, enqueues the tenant email in the same
// transaction (P3 sends it). The open month can't be issued.
func Issue(ctx context.Context, tx pgx.Tx, id, actor string, now time.Time) (before, after Invoice, err error) {
	if before, err = lockInvoice(ctx, tx, id); err != nil {
		return
	}
	if before.Status != StatusDraft {
		err = ErrNotDraft
		return
	}
	now = now.UTC()
	if now.Before(before.Month.AddDate(0, 1, 0)) {
		err = ErrMonthOpen
		return
	}
	number, err := nextNumber(ctx, tx, now.Year())
	if err != nil {
		return
	}
	if _, err = tx.Exec(ctx, `UPDATE invoices SET status = 'issued', number = $2, issued_at = $3, issued_by = $4 WHERE id = $1`,
		id, number, now, actor); err != nil {
		err = fmt.Errorf("issue: %w", err)
		return
	}
	on, err := emailsOn(ctx, tx)
	if err != nil {
		return
	}
	if on {
		if err = enqueueEmail(ctx, tx, id, "issued"); err != nil {
			err = fmt.Errorf("enqueue invoice email: %w", err)
			return
		}
	}
	after, err = loadInvoice(ctx, tx, id, false)
	return
}

// MarkPaid records an issued invoice as paid on paidAt (not in the future,
// not before the invoice's month). reference is a free-text note (≤ 200
// characters; never card or bank details by policy). A paid invoice is final.
func MarkPaid(ctx context.Context, tx pgx.Tx, id string, paidAt time.Time, reference, actor string, now time.Time) (before, after Invoice, err error) {
	reference = strings.TrimSpace(reference)
	if utf8.RuneCountInString(reference) > maxReferenceRunes {
		err = ErrReferenceTooLong
		return
	}
	if before, err = lockInvoice(ctx, tx, id); err != nil {
		return
	}
	if before.Status != StatusIssued {
		err = ErrNotIssued
		return
	}
	day := time.Date(paidAt.Year(), paidAt.Month(), paidAt.Day(), 0, 0, 0, 0, time.UTC)
	if day.After(now.UTC()) || day.Before(before.Month) {
		err = ErrInvalidPaidAt
		return
	}
	var ref *string
	if reference != "" {
		ref = &reference
	}
	if _, err = tx.Exec(ctx, `UPDATE invoices SET status = 'paid', paid_at = $2, paid_reference = $3, paid_by = $4 WHERE id = $1`,
		id, day, ref, actor); err != nil {
		err = fmt.Errorf("mark paid: %w", err)
		return
	}
	after, err = loadInvoice(ctx, tx, id, false)
	return
}

func checkReason(reason string) (string, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return "", ErrReasonRequired
	}
	if utf8.RuneCountInString(reason) > maxReasonRunes {
		return "", ErrReasonTooLong
	}
	return reason, nil
}

// Void voids an issued invoice (reason required) and, when email_tenants is
// on, enqueues the void notice. A correction is a void plus a regenerated
// draft. A draft is never voided (Discard deletes it) and a paid invoice is
// final: both are ErrNotIssued.
func Void(ctx context.Context, tx pgx.Tx, id, reason, actor string, now time.Time) (before, after Invoice, err error) {
	if reason, err = checkReason(reason); err != nil {
		return
	}
	if before, err = lockInvoice(ctx, tx, id); err != nil {
		return
	}
	if before.Status != StatusIssued {
		err = ErrNotIssued
		return
	}
	if _, err = tx.Exec(ctx, `UPDATE invoices SET status = 'void', voided_at = $2, voided_by = $3, void_reason = $4 WHERE id = $1`,
		id, now.UTC(), actor, reason); err != nil {
		err = fmt.Errorf("void: %w", err)
		return
	}
	on, err := emailsOn(ctx, tx)
	if err != nil {
		return
	}
	if on {
		if err = enqueueEmail(ctx, tx, id, "voided"); err != nil {
			err = fmt.Errorf("enqueue void notice: %w", err)
			return
		}
	}
	after, err = loadInvoice(ctx, tx, id, false)
	return
}

// Resend enqueues one more email for an issued, paid or void invoice (a
// super-admin's explicit resend, D-68): the invoice for issued and paid, the
// void notice for void (the sender follows the status). It is sent even when
// email_tenants is off: the operator asked for it.
func Resend(ctx context.Context, tx pgx.Tx, id string) (Invoice, error) {
	inv, err := lockInvoice(ctx, tx, id)
	if err != nil {
		return Invoice{}, err
	}
	if inv.Status == StatusDraft {
		return Invoice{}, ErrNotSent
	}
	if err := enqueueEmail(ctx, tx, id, "resend"); err != nil {
		return Invoice{}, fmt.Errorf("enqueue resend: %w", err)
	}
	return inv, nil
}

// PruneCutoff is the first month kept: the current UTC month minus the
// retention years (D-67). Invoices of earlier months are pruned.
func PruneCutoff(now time.Time, retentionYears int) time.Time {
	return MonthStart(now).AddDate(-retentionYears, 0, 0)
}

// Prune deletes invoices past the retention setting through prune_invoices()
// (the only path that deletes issued invoices; it re-checks the retention
// itself). audit runs in the same transaction with the count when > 0.
func Prune(ctx context.Context, db interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}, now time.Time, audit func(ctx context.Context, tx pgx.Tx, cutoff time.Time, n int64) error) (int64, error) {
	var n int64
	err := pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		s, err := GetSettings(ctx, tx)
		if err != nil {
			return err
		}
		cutoff := PruneCutoff(now, s.InvoiceRetentionYears)
		if err := tx.QueryRow(ctx, `SELECT prune_invoices($1::date)`, cutoff).Scan(&n); err != nil {
			return fmt.Errorf("prune invoices: %w", err)
		}
		if n > 0 && audit != nil {
			return audit(ctx, tx, cutoff, n)
		}
		return nil
	})
	return n, err
}
