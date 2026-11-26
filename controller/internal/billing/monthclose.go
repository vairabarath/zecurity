package billing

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourorg/ztna/controller/internal/providerquery"
)

// Month close (D-66): after each UTC month closes plus a settling delay
// (BILLING_SETTLE_DELAY, default 6 h), every tenant with a priced month-start
// card gets one draft invoice for it. Quantities come only from U2's
// ReadMonthUsage (D-59), the plan only from PlanForMonth (D-65), the card
// from CardForMonth (the revision in force on the 1st, D-63), tax from the
// current settings (D-69). A single controller runs the job (D-02).

// DefaultSettleDelay is the default BILLING_SETTLE_DELAY.
const DefaultSettleDelay = 6 * time.Hour

// catchUpMonths: closed months the job looks back at (a missed month is
// generated on start within this window).
const catchUpMonths = 3

// Generator drafts invoices.
type Generator struct {
	DB          *pgxpool.Pool
	SettleDelay time.Duration    // 0 = DefaultSettleDelay
	Now         func() time.Time // nil = time.Now
	// read is the usage reader; nil = providerquery.ReadMonthUsage. Tests
	// replace it to prove no other reader is used.
	read func(ctx context.Context, q providerquery.Querier, month time.Time) (providerquery.MonthUsage, error)
}

func (g *Generator) now() time.Time {
	if g.Now != nil {
		return g.Now().UTC()
	}
	return time.Now().UTC()
}

func (g *Generator) settle() time.Duration {
	if g.SettleDelay > 0 {
		return g.SettleDelay
	}
	return DefaultSettleDelay
}

func (g *Generator) readUsage(ctx context.Context, month time.Time) (providerquery.MonthUsage, error) {
	if g.read != nil {
		return g.read(ctx, g.DB, month)
	}
	return providerquery.ReadMonthUsage(ctx, g.DB, month)
}

// Run closes due months at start and then hourly until ctx ends.
func (g *Generator) Run(ctx context.Context) {
	tick := time.NewTicker(time.Hour)
	defer tick.Stop()
	for {
		if n, err := g.CloseDueMonths(ctx); err != nil {
			log.Printf("billing: month close: %v", err)
		} else if n > 0 {
			log.Printf("billing: month close: %d draft invoice(s) created", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// DueMonths are the closed months (newest last) whose settling delay has
// passed, within the catch-up window.
func (g *Generator) DueMonths() []time.Time {
	now := g.now()
	cur := MonthStart(now)
	var out []time.Time
	for i := catchUpMonths; i >= 1; i-- {
		m := cur.AddDate(0, -i, 0)
		if !now.Before(m.AddDate(0, 1, 0).Add(g.settle())) {
			out = append(out, m)
		}
	}
	return out
}

// CloseDueMonths drafts the missing invoices of every due month. It never
// touches an existing invoice; running it twice creates nothing new.
func (g *Generator) CloseDueMonths(ctx context.Context) (int, error) {
	total := 0
	for _, m := range g.DueMonths() {
		res, err := g.generate(ctx, m, "", false)
		total += res.Created
		if err != nil {
			return total, fmt.Errorf("month %s: %w", m.Format("2006-01"), err)
		}
	}
	return total, nil
}

// GenerateResult counts what a generation did.
type GenerateResult struct {
	Created     int
	Regenerated int
	// Changed: the drafts regenerated or created, before (nil for a new one)
	// and after, for the caller's audit.
	Changed []DraftChange
}

// DraftChange is one draft a regeneration wrote.
type DraftChange struct {
	Before *Invoice
	After  Invoice
}

// ErrMonthOpen: the month has not closed yet (issue and regenerate refuse it).
var ErrMonthOpen = errors.New("month not closed")

// Regenerate recomputes the drafts of a closed month (optionally one tenant)
// with the current cards' month-start revisions and settings, keeping each
// draft's id, and creates missing drafts (e.g. after a void). Issued, paid
// and void invoices are never touched.
func (g *Generator) Regenerate(ctx context.Context, month time.Time, tenantID string) (GenerateResult, error) {
	m := MonthStart(month)
	if g.now().Before(m.AddDate(0, 1, 0)) {
		return GenerateResult{}, ErrMonthOpen
	}
	return g.generate(ctx, m, tenantID, true)
}

type tenantMonth struct {
	id, name, slug string
	usage          providerquery.TenantMonthUsage
}

// billedTenants: every tenant ReadMonthUsage lists, plus tenants purged
// during the month without usage (a final invoice, D-67), minus tenants
// created after the month.
func (g *Generator) billedTenants(ctx context.Context, m time.Time, u providerquery.MonthUsage) ([]tenantMonth, error) {
	end := m.AddDate(0, 1, 0)
	seen := map[string]bool{}
	var out []tenantMonth
	for _, t := range u.Tenants {
		seen[t.Tenant.ID] = true
		out = append(out, tenantMonth{id: t.Tenant.ID, name: t.Tenant.Name, slug: t.Tenant.Slug, usage: t})
	}
	rows, err := g.DB.Query(ctx, `
SELECT tenant_id::text, name, slug FROM usage_tenants WHERE purged_at >= $1 AND purged_at < $2`, m, end)
	if err != nil {
		return nil, fmt.Errorf("purged tenants: %w", err)
	}
	for rows.Next() {
		var t tenantMonth
		if err := rows.Scan(&t.id, &t.name, &t.slug); err != nil {
			rows.Close()
			return nil, err
		}
		if !seen[t.id] {
			t.usage = providerquery.TenantMonthUsage{Meters: map[string]int64{}} // no usage counted
			out = append(out, t)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Created after the month: nothing to bill.
	late := map[string]bool{}
	rows, err = g.DB.Query(ctx, `SELECT id::text FROM workspaces WHERE created_at >= $1`, end)
	if err != nil {
		return nil, fmt.Errorf("late tenants: %w", err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		late[id] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	kept := out[:0]
	for _, t := range out {
		if !late[t.id] {
			kept = append(kept, t)
		}
	}
	return kept, nil
}

func (g *Generator) generate(ctx context.Context, m time.Time, onlyTenant string, regenerate bool) (GenerateResult, error) {
	var res GenerateResult
	u, err := g.readUsage(ctx, m)
	if err != nil {
		return res, err
	}
	settings, err := GetSettings(ctx, g.DB)
	if err != nil {
		return res, err
	}
	tenants, err := g.billedTenants(ctx, m, u)
	if err != nil {
		return res, err
	}
	var firstErr error
	for _, t := range tenants {
		if onlyTenant != "" && t.id != onlyTenant {
			continue
		}
		ch, created, err := g.draftOne(ctx, m, t, u, settings, regenerate)
		if err != nil {
			// One tenant's failure (e.g. an overflow) doesn't stop the
			// others; it is logged and retried on the next run.
			log.Printf("billing: tenant %s month %s: %v", t.id, m.Format("2006-01"), err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if ch == nil {
			continue
		}
		if created {
			res.Created++
		} else {
			res.Regenerated++
		}
		res.Changed = append(res.Changed, *ch)
	}
	if regenerate {
		return res, firstErr
	}
	return res, nil
}

// draftOne writes (or, when regenerating, rewrites) one tenant's draft in one
// transaction. Nothing happens when the month-start card is unpriced (D-62)
// or a non-draft invoice already exists.
func (g *Generator) draftOne(ctx context.Context, m time.Time, t tenantMonth, u providerquery.MonthUsage, s Settings, regenerate bool) (*DraftChange, bool, error) {
	bp, err := PlanForMonth(ctx, g.DB, t.id, m)
	if errors.Is(err, ErrNoBillingPlan) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	card, err := CardForMonth(ctx, g.DB, bp.PlanID, m)
	if errors.Is(err, ErrPlanNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if card.Card.Price == nil {
		return nil, false, nil // not billable (D-62)
	}
	comp, err := Compute(*card.Card.Price, t.usage.Value, s.TaxRateBP)
	if err != nil {
		return nil, false, err
	}
	inv := Invoice{
		TenantID: t.id, TenantName: t.name, TenantSlug: t.slug, Month: m, Status: StatusDraft,
		Currency: card.Card.Price.Currency, PlanID: bp.PlanID, PlanName: card.Card.Name, PlanRevision: card.Revision,
		SubtotalMinor: comp.Subtotal, TaxRateBP: s.TaxRateBP, TaxLabel: s.TaxLabel, TaxMinor: comp.Tax, TotalMinor: comp.Total,
		CollectedMinutes: u.CollectedMinutes, MonthMinutes: u.Minutes, IncompleteHours: u.IncompleteHours,
		Lines: comp.Lines,
	}

	var (
		change  *DraftChange
		created bool
	)
	err = pgx.BeginFunc(ctx, g.DB, func(tx pgx.Tx) error {
		existing, err := lockCurrentInvoice(ctx, tx, t.id, m)
		if err != nil && !errors.Is(err, ErrInvoiceNotFound) {
			return err
		}
		switch {
		case err == nil && existing.Status != StatusDraft:
			return nil // issued or paid: never touched
		case err == nil && !regenerate:
			return nil // the month close never rewrites a draft
		case err == nil:
			inv.ID = existing.ID
			if err := rewriteDraft(ctx, tx, inv); err != nil {
				return err
			}
			before := existing
			after, err := loadInvoice(ctx, tx, inv.ID, true)
			if err != nil {
				return err
			}
			change = &DraftChange{Before: &before, After: after}
			return nil
		}
		id, ok, err := insertDraftInvoice(ctx, tx, inv)
		if err != nil || !ok {
			return err
		}
		after, err := loadInvoice(ctx, tx, id, true)
		if err != nil {
			return err
		}
		change, created = &DraftChange{After: after}, true
		return nil
	})
	return change, created, err
}
