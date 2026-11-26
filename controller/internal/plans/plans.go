// Package plans implements provider-managed plans (Sprint 23 B1, F8;
// Decision Record D-43, quota keys of D-47).
//
// A plan has a name, a description and a limit for each of the six quota
// keys (Keys). A limit is a non-negative integer or nil (explicitly
// unlimited); a plan always defines all six keys, so a missing key is an
// error and never means unlimited. Exactly one plan is the default; a new
// workspace gets the current default from a database trigger (migration 062)
// unless its insert names a plan. No feature entitlements (a later
// decision).
//
// Sprint 26 P1b (F34, D-63, D-65): a plan is a priced card. Create and
// Update call billing.CreateCard / billing.SaveCard in their transaction
// (one edit = one revision, one audit row), and Assign records the
// month-start plan (billing.RecordMonthStartPlan) before the change, so a
// mid-month change bills from the next month. Limits behave as before.
//
// The store has no net/http dependency; the HTTP handlers live in
// handler.go. EffectiveLimits is the one definition of a workspace's limits:
// S3 (quotas, D-47) overlays its per-tenant overrides inside it.
package plans

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourorg/ztna/controller/internal/billing"
	"github.com/yourorg/ztna/controller/internal/quota"
)

// The six quota keys of D-47, in display order. Defined in internal/quota
// (Sprint 23 S3), which must stay free of net/http for providerquery.
const (
	KeyUsers          = quota.KeyUsers
	KeyResources      = quota.KeyResources
	KeyRemoteNetworks = quota.KeyRemoteNetworks
	KeyConnectors     = quota.KeyConnectors
	KeyShields        = quota.KeyShields
	KeyClientDevices  = quota.KeyClientDevices
)

// Keys lists every quota key. A plan defines all of them.
var Keys = quota.Keys

// Seeded plans (migration 062).
const (
	FreePlanID   = "00000000-0000-0000-0001-000000000001"
	LegacyPlanID = "00000000-0000-0000-0001-000000000002"
)

// Field bounds (match the migration's CHECKs).
const (
	MaxNameRunes        = 64
	MaxDescriptionRunes = 500
)

var (
	ErrNotFound          = errors.New("plan not found")
	ErrWorkspaceNotFound = errors.New("workspace not found")
	ErrNameTaken         = errors.New("plan name taken")
	ErrInUse             = errors.New("plan is assigned to a workspace")
	ErrIsDefault         = errors.New("plan is the default")
	ErrInvalidName       = errors.New("invalid plan name")
	ErrInvalidDesc       = errors.New("invalid plan description")
	ErrInvalidLimits     = errors.New("invalid plan limits")
	// ErrIncompleteLimits: a stored plan lacks a key. The migration's CHECK
	// prevents it; EffectiveLimits fails closed if it ever happens.
	ErrIncompleteLimits = errors.New("plan limits incomplete")
)

// CardMeta is who changes a card and why: the revision history row (D-63).
// An empty reason on create is stored as "Card created".
type CardMeta struct {
	Actor  string
	Reason string
}

func (m CardMeta) actor() string {
	if m.Actor == "" {
		return "system"
	}
	return m.Actor
}

// Limits maps every quota key to its limit; nil is unlimited.
type Limits map[string]*int64

// Validate reports whether l defines exactly the six keys, each nil or >= 0.
func (l Limits) Validate() error {
	if len(l) != len(Keys) {
		return ErrInvalidLimits
	}
	for _, k := range Keys {
		v, ok := l[k]
		if !ok || (v != nil && *v < 0) {
			return ErrInvalidLimits
		}
	}
	return nil
}

// Clone returns a copy that shares no pointers with l.
func (l Limits) Clone() Limits {
	out := make(Limits, len(l))
	for k, v := range l {
		if v != nil {
			n := *v
			out[k] = &n
		} else {
			out[k] = nil
		}
	}
	return out
}

func parseLimits(raw []byte) (Limits, error) {
	var l Limits
	if err := json.Unmarshal(raw, &l); err != nil {
		return nil, fmt.Errorf("decode plan limits: %w", err)
	}
	if err := l.Validate(); err != nil {
		return nil, ErrIncompleteLimits
	}
	return l, nil
}

// Plan is one row of plans.
type Plan struct {
	ID          string
	Name        string
	Description string
	Limits      Limits
	IsDefault   bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
	// Revision is the card revision (D-63); Price its price part (nil = not
	// billable). Price is filled by the card writes (Create, Update).
	Revision int
	Price    *billing.Price
}

// AuditView is the plan as written into audit details (before/after).
func (p Plan) AuditView() map[string]any {
	return map[string]any{
		"name":        p.Name,
		"description": p.Description,
		"limits":      p.Limits,
		"is_default":  p.IsDefault,
		"price":       p.Price,
		"revision":    p.Revision,
	}
}

// Querier is satisfied by *pgxpool.Pool and pgx.Tx.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Store reads and writes plans.
type Store struct {
	pool *pgxpool.Pool
	// Now is the clock for the month-start plan (nil = time.Now); tests set it.
	Now func() time.Time
}

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

const planColumns = `id::text, name, description, limits, is_default, created_at, updated_at, revision`

func scanPlan(row pgx.Row) (Plan, error) {
	var p Plan
	var raw []byte
	if err := row.Scan(&p.ID, &p.Name, &p.Description, &raw, &p.IsDefault, &p.CreatedAt, &p.UpdatedAt, &p.Revision); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Plan{}, ErrNotFound
		}
		return Plan{}, err
	}
	l, err := parseLimits(raw)
	if err != nil {
		return Plan{}, err
	}
	p.Limits = l
	p.CreatedAt, p.UpdatedAt = p.CreatedAt.UTC(), p.UpdatedAt.UTC()
	return p, nil
}

// normalizeName trims and checks a plan name.
func normalizeName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" || utf8.RuneCountInString(name) > MaxNameRunes || strings.ContainsFunc(name, isControl) {
		return "", ErrInvalidName
	}
	return name, nil
}

func normalizeDescription(raw string) (string, error) {
	d := strings.TrimSpace(raw)
	if utf8.RuneCountInString(d) > MaxDescriptionRunes || strings.ContainsFunc(d, func(r rune) bool { return isControl(r) && r != '\n' }) {
		return "", ErrInvalidDesc
	}
	return d, nil
}

func isControl(r rune) bool { return r < 0x20 || r == 0x7f }

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func limitsJSON(l Limits) ([]byte, error) {
	if err := l.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(l)
}

// inTx runs fn in a transaction and commits.
func (s *Store) inTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Create inserts a plan card (never the default; use SetDefault) with an
// optional price (nil = not billable) and its first revision. audit runs in
// the same transaction.
func (s *Store) Create(ctx context.Context, name, description string, limits Limits, price *billing.Price, meta CardMeta,
	audit func(ctx context.Context, tx pgx.Tx, p Plan) error) (Plan, error) {
	name, err := normalizeName(name)
	if err != nil {
		return Plan{}, err
	}
	desc, err := normalizeDescription(description)
	if err != nil {
		return Plan{}, err
	}
	raw, err := limitsJSON(limits)
	if err != nil {
		return Plan{}, err
	}
	var p Plan
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		p, err = scanPlan(tx.QueryRow(ctx,
			`INSERT INTO plans (name, description, limits) VALUES ($1, $2, $3) RETURNING `+planColumns,
			name, desc, raw))
		if err != nil {
			if isUniqueViolation(err) {
				return ErrNameTaken
			}
			return fmt.Errorf("insert plan: %w", err)
		}
		if err := checkPriceCurrency(ctx, tx, price); err != nil {
			return err
		}
		reason := meta.Reason
		if reason == "" {
			reason = "Card created"
		}
		card, err := billing.CreateCard(ctx, tx, p.ID, price, meta.actor(), reason)
		if err != nil {
			return err
		}
		p.Price = card.Price
		if audit != nil {
			return audit(ctx, tx, p)
		}
		return nil
	})
	return p, err
}

// checkPriceCurrency validates a price against the billing currency (D-64).
// The settings row is read FOR SHARE, so a concurrent currency switch (which
// locks it FOR UPDATE) waits for this card write, or this write waits for it
// and then sees the new currency.
func checkPriceCurrency(ctx context.Context, tx pgx.Tx, price *billing.Price) error {
	var currency string
	if err := tx.QueryRow(ctx, `SELECT currency FROM billing_settings FOR SHARE`).Scan(&currency); err != nil {
		return fmt.Errorf("read billing currency: %w", err)
	}
	return billing.ValidatePrice(price, currency)
}

// Change is a partial update; nil fields are unchanged. Limits, when set,
// replaces all six values. Price.Set=false leaves the price unchanged;
// Price.Set with a nil Price removes it.
type Change struct {
	Name        *string
	Description *string
	Limits      Limits
	Price       billing.PriceChange
}

// Update applies a change under the plan's row lock: plan fields, price, one
// new card revision (D-63). changed=false (no revision, no audit) when
// nothing differs. audit gets the plan before and after.
func (s *Store) Update(ctx context.Context, id string, c Change, meta CardMeta,
	audit func(ctx context.Context, tx pgx.Tx, before, after Plan) error) (changed bool, err error) {
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		before, err := scanPlan(tx.QueryRow(ctx, `SELECT `+planColumns+` FROM plans WHERE id = $1 FOR UPDATE`, id))
		if err != nil {
			return err
		}
		after := before
		after.Limits = before.Limits.Clone()
		if c.Name != nil {
			if after.Name, err = normalizeName(*c.Name); err != nil {
				return err
			}
		}
		if c.Description != nil {
			if after.Description, err = normalizeDescription(*c.Description); err != nil {
				return err
			}
		}
		if c.Limits != nil {
			if err := c.Limits.Validate(); err != nil {
				return err
			}
			after.Limits = c.Limits.Clone()
		}
		if before.Price, err = billing.CardPrice(ctx, tx, id); err != nil {
			return err
		}
		after.Price = before.Price
		if c.Price.Set {
			if err := checkPriceCurrency(ctx, tx, c.Price.Price); err != nil {
				return err
			}
			after.Price = c.Price.Price
		}
		priceChanged := !billing.SamePrice(before.Price, after.Price)
		if after.Name == before.Name && after.Description == before.Description && sameLimits(after.Limits, before.Limits) && !priceChanged {
			return nil
		}
		raw, err := limitsJSON(after.Limits)
		if err != nil {
			return err
		}
		after, err = scanPlan(tx.QueryRow(ctx,
			`UPDATE plans SET name = $2, description = $3, limits = $4, updated_at = NOW()
			  WHERE id = $1 RETURNING `+planColumns, id, after.Name, after.Description, raw))
		if err != nil {
			if isUniqueViolation(err) {
				return ErrNameTaken
			}
			return fmt.Errorf("update plan: %w", err)
		}
		card, rev, err := billing.SaveCard(ctx, tx, id, billing.PriceChange{Set: priceChanged, Price: c.Price.Price}, meta.actor(), meta.Reason)
		if err != nil {
			return err
		}
		after.Price, after.Revision = card.Price, rev
		changed = true
		if audit != nil {
			return audit(ctx, tx, before, after)
		}
		return nil
	})
	return changed, err
}

func sameLimits(a, b Limits) bool {
	for _, k := range Keys {
		x, y := a[k], b[k]
		if (x == nil) != (y == nil) || (x != nil && *x != *y) {
			return false
		}
	}
	return true
}

// Delete removes a plan that is neither the default nor assigned to any
// workspace. The plan row is locked first, so a concurrent assignment (which
// locks the plan row with FOR KEY SHARE through its foreign key) can't slip
// in between the check and the delete.
func (s *Store) Delete(ctx context.Context, id string,
	audit func(ctx context.Context, tx pgx.Tx, before Plan) error) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		before, err := scanPlan(tx.QueryRow(ctx, `SELECT `+planColumns+` FROM plans WHERE id = $1 FOR UPDATE`, id))
		if err != nil {
			return err
		}
		if before.IsDefault {
			return ErrIsDefault
		}
		var used bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM workspaces WHERE plan_id = $1)`, id).Scan(&used); err != nil {
			return fmt.Errorf("check plan use: %w", err)
		}
		if used {
			return ErrInUse
		}
		if _, err := tx.Exec(ctx, `DELETE FROM plans WHERE id = $1`, id); err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23503" { // foreign key: assigned meanwhile
				return ErrInUse
			}
			return fmt.Errorf("delete plan: %w", err)
		}
		if audit != nil {
			return audit(ctx, tx, before)
		}
		return nil
	})
}

// SetDefault makes a plan the default. changed=false (no audit) when it
// already is. audit gets the previous default and the new one.
func (s *Store) SetDefault(ctx context.Context, id string,
	audit func(ctx context.Context, tx pgx.Tx, previous, next Plan) error) (changed bool, err error) {
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		// Lock every plan row in id order. The set must not depend on which
		// plan is the default: that changes while concurrent callers wait, so
		// "the default plus the target" could be locked in different orders
		// and deadlock. Plans are few, so locking them all is cheap, and
		// concurrent set_default calls serialise.
		rows, err := tx.Query(ctx, `SELECT id FROM plans ORDER BY id FOR UPDATE`)
		if err != nil {
			return fmt.Errorf("lock plans: %w", err)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("lock plans: %w", err)
		}
		next, err := scanPlan(tx.QueryRow(ctx, `SELECT `+planColumns+` FROM plans WHERE id = $1`, id))
		if err != nil {
			return err
		}
		if next.IsDefault {
			return nil
		}
		previous, err := scanPlan(tx.QueryRow(ctx, `SELECT `+planColumns+` FROM plans WHERE is_default`))
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE plans SET is_default = false, updated_at = NOW() WHERE is_default`); err != nil {
			return fmt.Errorf("clear default plan: %w", err)
		}
		if next, err = scanPlan(tx.QueryRow(ctx,
			`UPDATE plans SET is_default = true, updated_at = NOW() WHERE id = $1 RETURNING `+planColumns, id)); err != nil {
			return fmt.Errorf("set default plan: %w", err)
		}
		changed = true
		if audit != nil {
			return audit(ctx, tx, previous, next)
		}
		return nil
	})
	return changed, err
}

// Assign sets a workspace's plan; it takes effect immediately on
// EffectiveLimits. Lowering limits never deletes anything (D-43). changed is
// false (no audit) when the workspace already has the plan. audit gets the
// plan before and after.
func (s *Store) Assign(ctx context.Context, workspaceID, planID string,
	audit func(ctx context.Context, tx pgx.Tx, before, after Plan) error) (changed bool, err error) {
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		var currentID string
		err := tx.QueryRow(ctx, `SELECT plan_id::text FROM workspaces WHERE id = $1 FOR UPDATE`, workspaceID).Scan(&currentID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrWorkspaceNotFound
		}
		if err != nil {
			return fmt.Errorf("lock workspace: %w", err)
		}
		after, err := scanPlan(tx.QueryRow(ctx, `SELECT `+planColumns+` FROM plans WHERE id = $1 FOR SHARE`, planID))
		if err != nil {
			return err
		}
		if currentID == planID {
			return nil
		}
		before, err := scanPlan(tx.QueryRow(ctx, `SELECT `+planColumns+` FROM plans WHERE id = $1`, currentID))
		if err != nil {
			return fmt.Errorf("read current plan: %w", err)
		}
		// D-65: the plan being replaced is the month's start plan if this is
		// the first change this month; recorded before the change.
		if err := billing.RecordMonthStartPlan(ctx, tx, workspaceID, currentID, s.now()); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE workspaces SET plan_id = $2, updated_at = NOW() WHERE id = $1`, workspaceID, planID); err != nil {
			return fmt.Errorf("assign plan: %w", err)
		}
		changed = true
		if audit != nil {
			return audit(ctx, tx, before, after)
		}
		return nil
	})
	return changed, err
}

// Get reads one plan.
func Get(ctx context.Context, q Querier, id string) (Plan, error) {
	return scanPlan(q.QueryRow(ctx, `SELECT `+planColumns+` FROM plans WHERE id = $1`, id))
}

// EffectiveLimits returns a workspace's limits: its plan's values (D-43)
// overlaid with its per-tenant overrides (S3, D-47). The definition is
// quota.Effective, which every quota check uses. Run it inside the creating
// transaction (q = tx) so the check is atomic with the creation. A missing
// workspace or key is an error: callers refuse the creation (fail closed).
func EffectiveLimits(ctx context.Context, q quota.Querier, workspaceID string) (Limits, error) {
	l, err := quota.Effective(ctx, q, workspaceID)
	switch {
	case errors.Is(err, quota.ErrWorkspaceNotFound):
		return nil, ErrWorkspaceNotFound
	case errors.Is(err, quota.ErrIncompleteLimits):
		return nil, ErrIncompleteLimits
	case err != nil:
		return nil, err
	}
	return Limits(quota.Values(l)), nil
}
