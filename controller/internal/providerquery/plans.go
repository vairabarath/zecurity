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
       p.created_at, p.updated_at
  FROM plans p`

func scanPlanSummary(row pgx.Row) (PlanSummary, error) {
	var s PlanSummary
	var raw []byte
	if err := row.Scan(&s.ID, &s.Name, &s.Description, &raw, &s.IsDefault, &s.Tenants, &s.CreatedAt, &s.UpdatedAt); err != nil {
		return PlanSummary{}, err
	}
	if err := json.Unmarshal(raw, &s.Limits); err != nil {
		return PlanSummary{}, fmt.Errorf("decode plan limits: %w", err)
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
