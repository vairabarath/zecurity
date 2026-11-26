package billing

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// The month-start plan (D-65): a tenant is billed for the whole month at the
// plan it had on the 1st. A mid-month change bills from the next month.
// PlanForMonth is the ONE billing-plan rule; nothing else derives it.

// ErrNoBillingPlan: the tenant has no plan for the month (no workspace, no
// usage snapshot with a plan).
var ErrNoBillingPlan = errors.New("no billing plan for the tenant and month")

// RecordMonthStartPlan runs in B1's assign transaction BEFORE the plan
// changes, with the workspace row locked. It records the plan being replaced
// as the month's start plan; ON CONFLICT DO NOTHING keeps the first one, so
// later changes in the same month never overwrite it.
func RecordMonthStartPlan(ctx context.Context, tx pgx.Tx, tenantID, oldPlanID string, now time.Time) error {
	if _, err := tx.Exec(ctx, `
INSERT INTO tenant_billing_plans (tenant_id, month, plan_id) VALUES ($1, $2, $3)
ON CONFLICT (tenant_id, month) DO NOTHING`, tenantID, MonthStart(now), oldPlanID); err != nil {
		return fmt.Errorf("record month-start plan: %w", err)
	}
	return nil
}

// BillingPlan is the plan a tenant's month is billed at.
type BillingPlan struct {
	PlanID string
	// Source: "recorded" (the month had a plan change; its start plan),
	// "later_change" (no change in the month; the start plan recorded by the
	// next month with a change, which is the plan the tenant kept until then),
	// "current" (the live workspace's plan), "frozen" (a purged tenant's
	// snapshot).
	Source string
}

// PlanForMonth returns the plan month is billed at:
//  1. the month's tenant_billing_plans row (its first change recorded the
//     plan it replaced);
//  2. else the earliest row of a LATER month: with no change in between, the
//     plan at the start of that month is the plan the tenant had all along,
//     so a month regenerated after a later change still bills its own plan;
//  3. else the live workspace's plan (no change since);
//  4. else (purged) the plan frozen in usage_tenants.
func PlanForMonth(ctx context.Context, q Querier, tenantID string, month time.Time) (BillingPlan, error) {
	start := MonthStart(month)
	var (
		planID   string
		recorded time.Time
	)
	err := q.QueryRow(ctx, `
SELECT plan_id::text, month FROM tenant_billing_plans
 WHERE tenant_id = $1 AND month >= $2
 ORDER BY month LIMIT 1`, tenantID, start).Scan(&planID, &recorded)
	switch {
	case err == nil:
		src := "later_change"
		if recorded.Equal(start) {
			src = "recorded"
		}
		return BillingPlan{PlanID: planID, Source: src}, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return BillingPlan{}, fmt.Errorf("month-start plan: %w", err)
	}
	err = q.QueryRow(ctx, `SELECT plan_id::text FROM workspaces WHERE id = $1`, tenantID).Scan(&planID)
	switch {
	case err == nil:
		return BillingPlan{PlanID: planID, Source: "current"}, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return BillingPlan{}, fmt.Errorf("current plan: %w", err)
	}
	var frozen *string
	err = q.QueryRow(ctx, `SELECT plan_id::text FROM usage_tenants WHERE tenant_id = $1`, tenantID).Scan(&frozen)
	switch {
	case err == nil && frozen != nil:
		return BillingPlan{PlanID: *frozen, Source: "frozen"}, nil
	case err == nil, errors.Is(err, pgx.ErrNoRows):
		return BillingPlan{}, ErrNoBillingPlan
	default:
		return BillingPlan{}, fmt.Errorf("frozen plan: %w", err)
	}
}

// PlanName names a plan for display: the plan's name, or (a plan deleted
// since) the name in its last card revision.
func PlanName(ctx context.Context, q Querier, planID string) (string, error) {
	var name string
	err := q.QueryRow(ctx, `
SELECT coalesce((SELECT name FROM plans WHERE id = $1),
                (SELECT card ->> 'name' FROM plan_card_revisions WHERE plan_id = $1 ORDER BY revision DESC LIMIT 1), '')`,
		planID).Scan(&name)
	if err != nil {
		return "", fmt.Errorf("plan name: %w", err)
	}
	return name, nil
}
