package providerquery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Plan reads (Sprint 23 B1, D-43). Plans are super-admin data (plan.read).
// Limits are passed through as stored: an object with the six quota keys of
// D-47, each a non-negative integer or null (unlimited). This package
// doesn't import internal/plans, which serves HTTP (D-04).

// PlanLimits is a plan's limits, key → limit (nil = unlimited).
type PlanLimits map[string]*int64

// PlanSummary is one row of GET /provider/plans.
type PlanSummary struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Description string     `json:"description"`
	Limits      PlanLimits `json:"limits"`
	IsDefault   bool       `json:"is_default"`
	// Tenants: workspaces on this plan.
	Tenants   int64     `json:"tenants"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// Price is the card's price (Sprint 26 P1b, D-63); null = not billable.
	Price *PlanPrice `json:"price"`
	// Revision is the card revision; every edit raises it (D-63).
	Revision int `json:"revision"`
}

// PlanPrice is a card's price part: minor units of Currency (D-64).
type PlanPrice struct {
	Currency     string                    `json:"currency"`
	BaseFeeMinor int64                     `json:"base_fee_minor"`
	Meters       map[string]PlanMeterPrice `json:"meters"`
}

// PlanMeterPrice prices one meter above its included quantity.
type PlanMeterPrice struct {
	Included       int64 `json:"included"`
	Unit           int64 `json:"unit"`
	UnitPriceMinor int64 `json:"unit_price_minor"`
}

// PlanRevision is one row of GET /provider/plans/{id}/revisions: the whole
// card after a change. ChangedBy is the operator email or "system".
type PlanRevision struct {
	Revision  int             `json:"revision"`
	ChangedAt time.Time       `json:"changed_at"`
	ChangedBy string          `json:"changed_by"`
	Reason    string          `json:"reason"`
	Card      json.RawMessage `json:"card"`
}

// TenantPlan is the plan section of tenant detail.
type TenantPlan struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Limits    PlanLimits `json:"limits"`
	IsDefault bool       `json:"is_default"`
}

const planSummarySelect = `
SELECT p.id::text, p.name, p.description, p.limits, p.is_default,
       (SELECT count(*) FROM workspaces WHERE plan_id = p.id),
       p.created_at, p.updated_at, p.revision,
       pp.currency, pp.base_fee_minor, pp.meters
  FROM plans p LEFT JOIN plan_prices pp ON pp.plan_id = p.id`

func scanPlanSummary(row pgx.Row) (PlanSummary, error) {
	var s PlanSummary
	var (
		raw, meters []byte
		currency    *string
		baseFee     *int64
	)
	if err := row.Scan(&s.ID, &s.Name, &s.Description, &raw, &s.IsDefault, &s.Tenants, &s.CreatedAt, &s.UpdatedAt,
		&s.Revision, &currency, &baseFee, &meters); err != nil {
		return PlanSummary{}, err
	}
	if err := json.Unmarshal(raw, &s.Limits); err != nil {
		return PlanSummary{}, fmt.Errorf("decode plan limits: %w", err)
	}
	if currency != nil && baseFee != nil {
		s.Price = &PlanPrice{Currency: *currency, BaseFeeMinor: *baseFee, Meters: map[string]PlanMeterPrice{}}
		if err := json.Unmarshal(meters, &s.Price.Meters); err != nil {
			return PlanSummary{}, fmt.Errorf("decode plan price: %w", err)
		}
	}
	s.CreatedAt, s.UpdatedAt = s.CreatedAt.UTC(), s.UpdatedAt.UTC()
	return s, nil
}

// ListPlans returns every plan, the default first, then by name. Plans are
// few (operator-created); no pagination.
func ListPlans(ctx context.Context, q Querier) ([]PlanSummary, error) {
	rows, err := q.Query(ctx, planSummarySelect+` ORDER BY p.is_default DESC, p.name, p.id`)
	if err != nil {
		return nil, fmt.Errorf("list plans: %w", err)
	}
	defer rows.Close()
	out := []PlanSummary{}
	for rows.Next() {
		s, err := scanPlanSummary(rows)
		if err != nil {
			return nil, fmt.Errorf("scan plan: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// GetPlan reads one plan.
func GetPlan(ctx context.Context, q Querier, id string) (PlanSummary, error) {
	if !IsUUID(id) {
		return PlanSummary{}, ErrInvalidID
	}
	s, err := scanPlanSummary(q.QueryRow(ctx, planSummarySelect+` WHERE p.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return PlanSummary{}, ErrNotFound
	}
	if err != nil {
		return PlanSummary{}, fmt.Errorf("get plan: %w", err)
	}
	return s, nil
}

// tenantPlan reads a tenant's plan.
func tenantPlan(ctx context.Context, q Querier, workspaceID string) (TenantPlan, error) {
	var t TenantPlan
	var raw []byte
	err := q.QueryRow(ctx, `
SELECT p.id::text, p.name, p.limits, p.is_default
  FROM workspaces w JOIN plans p ON p.id = w.plan_id
 WHERE w.id = $1`, workspaceID).Scan(&t.ID, &t.Name, &raw, &t.IsDefault)
	if err != nil {
		return TenantPlan{}, fmt.Errorf("tenant plan: %w", err)
	}
	if err := json.Unmarshal(raw, &t.Limits); err != nil {
		return TenantPlan{}, fmt.Errorf("decode tenant plan limits: %w", err)
	}
	return t, nil
}

// ListPlanRevisions returns a card's history, newest first (D-63). A deleted
// plan keeps its history, so an unknown id with revisions still reads; an id
// with neither a plan nor revisions is ErrNotFound.
func ListPlanRevisions(ctx context.Context, q Querier, planID string) ([]PlanRevision, error) {
	if !IsUUID(planID) {
		return nil, ErrInvalidID
	}
	rows, err := q.Query(ctx, `
SELECT revision, changed_at, changed_by, reason, card FROM plan_card_revisions
 WHERE plan_id = $1 ORDER BY revision DESC`, planID)
	if err != nil {
		return nil, fmt.Errorf("list plan revisions: %w", err)
	}
	defer rows.Close()
	out := []PlanRevision{}
	for rows.Next() {
		var r PlanRevision
		if err := rows.Scan(&r.Revision, &r.ChangedAt, &r.ChangedBy, &r.Reason, &r.Card); err != nil {
			return nil, fmt.Errorf("scan plan revision: %w", err)
		}
		r.ChangedAt = r.ChangedAt.UTC()
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, ErrNotFound
	}
	return out, nil
}
