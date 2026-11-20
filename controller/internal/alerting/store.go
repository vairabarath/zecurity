package alerting

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	// ErrNotFound: no such rule.
	ErrNotFound = errors.New("alerting: not found")
	// ErrInvalidRule: a field is out of range (name, severity, timing).
	ErrInvalidRule = errors.New("alerting: invalid rule")
	// ErrNameTaken: another rule has that name.
	ErrNameTaken = errors.New("alerting: rule name taken")
)

// Rule is one alert rule.
type Rule struct {
	ID              string    `json:"id"`
	Kind            string    `json:"kind"`
	Scope           string    `json:"scope"`
	Name            string    `json:"name"`
	Enabled         bool      `json:"enabled"`
	Severity        string    `json:"severity"`
	Params          Params    `json:"params"`
	ForSeconds      int       `json:"for_seconds"`
	RenotifySeconds int       `json:"renotify_seconds"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// AuditView is the rule as recorded in the provider audit (before/after).
func (r Rule) AuditView() map[string]any {
	return map[string]any{
		"kind": r.Kind, "scope": r.Scope, "name": r.Name, "enabled": r.Enabled, "severity": r.Severity,
		"params": r.Params, "for_seconds": r.ForSeconds, "renotify_seconds": r.RenotifySeconds,
	}
}

// Validate checks the fields the database also checks, so the API answers
// with a clear error instead of a constraint violation.
func (r Rule) Validate() error {
	name := strings.TrimSpace(r.Name)
	switch {
	case ScopeOf(r.Kind) == "":
		return fmt.Errorf("%w: unknown kind", ErrInvalidRule)
	case r.Scope != ScopeOf(r.Kind):
		return fmt.Errorf("%w: scope doesn't match kind", ErrInvalidRule)
	case name == "" || utf8.RuneCountInString(name) > 100 || name != r.Name:
		return fmt.Errorf("%w: name must be 1..100 characters without surrounding spaces", ErrInvalidRule)
	case !ValidSeverity(r.Severity):
		return fmt.Errorf("%w: unknown severity", ErrInvalidRule)
	case r.ForSeconds < 0 || r.ForSeconds > MaxForSeconds:
		return fmt.Errorf("%w: for_seconds must be 0..%d", ErrInvalidRule, MaxForSeconds)
	case r.RenotifySeconds != 0 && (r.RenotifySeconds < MinRenotifySeconds || r.RenotifySeconds > MaxRenotifySeconds):
		return fmt.Errorf("%w: renotify_seconds must be 0 or %d..%d", ErrInvalidRule, MinRenotifySeconds, MaxRenotifySeconds)
	}
	for _, c := range r.Name {
		if c < 0x20 || c == 0x7f {
			return fmt.Errorf("%w: name has control characters", ErrInvalidRule)
		}
	}
	return nil
}

// Event is one transition in alert_events: what A2 delivers.
type Event struct {
	ID         string    `json:"id"`
	RuleID     string    `json:"rule_id"`
	Kind       string    `json:"kind"`
	Scope      string    `json:"scope"`
	TenantID   *string   `json:"tenant_id"`
	TargetType string    `json:"target_type"`
	TargetID   string    `json:"target_id"`
	TargetName string    `json:"target_name"`
	Transition string    `json:"transition"` // fired | renotified | resolved
	Status     string    `json:"status"`     // firing | resolved
	Severity   string    `json:"severity"`
	Summary    string    `json:"summary"`
	StartedAt  time.Time `json:"started_at"`
	OccurredAt time.Time `json:"occurred_at"`
}

// Transitions.
const (
	TransitionFired      = "fired"
	TransitionRenotified = "renotified"
	TransitionResolved   = "resolved"
)

// Store reads and writes alert rules, states and events.
type Store struct{ pool *pgxpool.Pool }

// NewStore returns a Store on pool.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

const ruleColumns = `id::text, kind, scope, name, enabled, severity, params, for_seconds, renotify_seconds, created_at, updated_at`

func scanRule(row pgx.Row) (Rule, error) {
	var r Rule
	var raw []byte
	if err := row.Scan(&r.ID, &r.Kind, &r.Scope, &r.Name, &r.Enabled, &r.Severity, &raw,
		&r.ForSeconds, &r.RenotifySeconds, &r.CreatedAt, &r.UpdatedAt); err != nil {
		return Rule{}, err
	}
	p, err := ParseParams(r.Kind, raw)
	if err != nil {
		return Rule{}, fmt.Errorf("rule %s: stored params: %w", r.ID, err)
	}
	r.Params = p
	r.CreatedAt, r.UpdatedAt = r.CreatedAt.UTC(), r.UpdatedAt.UTC()
	return r, nil
}

// ListRules returns every rule, platform first, then by name. A row whose
// stored params no longer parse is logged and skipped, so one bad rule
// can't stop every other rule from being evaluated.
func (s *Store) ListRules(ctx context.Context) ([]Rule, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+ruleColumns+` FROM alert_rules ORDER BY scope, name, id`)
	if err != nil {
		return nil, fmt.Errorf("list alert rules: %w", err)
	}
	defer rows.Close()
	var out []Rule
	for rows.Next() {
		r, err := scanRule(rows)
		if errors.Is(err, ErrInvalidParams) {
			log.Printf("alerting: skipping rule with invalid stored params: %v", err)
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("list alert rules: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetRule returns one rule.
func (s *Store) GetRule(ctx context.Context, id string) (Rule, error) {
	r, err := scanRule(s.pool.QueryRow(ctx, `SELECT `+ruleColumns+` FROM alert_rules WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Rule{}, ErrNotFound
	}
	return r, err
}

// AuditFunc writes the provider audit row in the same transaction as the
// change (F17: the change and its audit commit together).
type AuditFunc func(ctx context.Context, tx pgx.Tx, before, after *Rule) error

func (s *Store) inTx(ctx context.Context, fn func(pgx.Tx) error) error {
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

func isUniqueViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}

// CreateRule inserts a rule (its scope follows its kind).
func (s *Store) CreateRule(ctx context.Context, r Rule, audit AuditFunc) (Rule, error) {
	r.Scope = ScopeOf(r.Kind)
	if err := r.Validate(); err != nil {
		return Rule{}, err
	}
	var out Rule
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		out, err = scanRule(tx.QueryRow(ctx, `
INSERT INTO alert_rules (kind, scope, name, enabled, severity, params, for_seconds, renotify_seconds)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING `+ruleColumns,
			r.Kind, r.Scope, r.Name, r.Enabled, r.Severity, r.Params.JSON(), r.ForSeconds, r.RenotifySeconds))
		if isUniqueViolation(err) {
			return ErrNameTaken
		}
		if err != nil {
			return fmt.Errorf("create alert rule: %w", err)
		}
		return audit(ctx, tx, nil, &out)
	})
	return out, err
}

// RuleChange is a partial update; nil fields are unchanged. Params is raw
// JSON validated against the rule's kind.
type RuleChange struct {
	Name            *string
	Enabled         *bool
	Severity        *string
	Params          json.RawMessage
	ForSeconds      *int
	RenotifySeconds *int
}

// UpdateRule applies change under a row lock. Disabling a rule resolves its
// alerts on the next evaluator tick (not here), so every resolution goes
// through one path.
func (s *Store) UpdateRule(ctx context.Context, id string, c RuleChange, audit AuditFunc) (Rule, error) {
	var out Rule
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		before, err := scanRule(tx.QueryRow(ctx, `SELECT `+ruleColumns+` FROM alert_rules WHERE id = $1 FOR UPDATE`, id))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		after := before
		if c.Name != nil {
			after.Name = *c.Name
		}
		if c.Enabled != nil {
			after.Enabled = *c.Enabled
		}
		if c.Severity != nil {
			after.Severity = *c.Severity
		}
		if c.ForSeconds != nil {
			after.ForSeconds = *c.ForSeconds
		}
		if c.RenotifySeconds != nil {
			after.RenotifySeconds = *c.RenotifySeconds
		}
		if c.Params != nil {
			p, err := ParseParams(after.Kind, c.Params)
			if err != nil {
				return err
			}
			after.Params = p
		}
		if err := after.Validate(); err != nil {
			return err
		}
		out, err = scanRule(tx.QueryRow(ctx, `
UPDATE alert_rules SET name = $2, enabled = $3, severity = $4, params = $5, for_seconds = $6,
       renotify_seconds = $7, updated_at = NOW()
 WHERE id = $1 RETURNING `+ruleColumns,
			id, after.Name, after.Enabled, after.Severity, after.Params.JSON(), after.ForSeconds, after.RenotifySeconds))
		if isUniqueViolation(err) {
			return ErrNameTaken
		}
		if err != nil {
			return fmt.Errorf("update alert rule: %w", err)
		}
		return audit(ctx, tx, &before, &out)
	})
	return out, err
}

// DeleteRule removes a rule. Its firing alerts are resolved in the same
// transaction (a "resolved" event each), so no alert is left firing without
// a rule; the events are returned for delivery.
func (s *Store) DeleteRule(ctx context.Context, id string, audit AuditFunc, now time.Time) ([]Event, error) {
	var events []Event
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		before, err := scanRule(tx.QueryRow(ctx, `SELECT `+ruleColumns+` FROM alert_rules WHERE id = $1 FOR UPDATE`, id))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		states, err := loadStates(ctx, tx, id)
		if err != nil {
			return err
		}
		for _, st := range states {
			if st.Status != statusFiring {
				continue
			}
			ev, err := insertEvent(ctx, tx, before, st, TransitionResolved, "Resolved (rule deleted): "+st.Summary, now)
			if err != nil {
				return err
			}
			events = append(events, ev)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM alert_rules WHERE id = $1`, id); err != nil {
			return fmt.Errorf("delete alert rule: %w", err)
		}
		return audit(ctx, tx, &before, nil)
	})
	if err != nil {
		return nil, err
	}
	return events, nil
}

// State statuses.
const (
	statusPending = "pending"
	statusFiring  = "firing"
)

// state is one alert_states row.
type state struct {
	TargetType     string
	TargetID       string
	Scope          string
	TenantID       *string
	TargetName     string
	Status         string
	Severity       string
	Summary        string
	PendingSince   time.Time
	FiringSince    *time.Time
	LastNotifiedAt *time.Time
}

func (st state) key() string { return st.TargetType + "\x00" + st.TargetID }

func loadStates(ctx context.Context, tx pgx.Tx, ruleID string) (map[string]state, error) {
	rows, err := tx.Query(ctx, `
SELECT target_type, target_id, scope, tenant_id::text, target_name, status, severity, summary,
       pending_since, firing_since, last_notified_at
  FROM alert_states WHERE rule_id = $1 FOR UPDATE`, ruleID)
	if err != nil {
		return nil, fmt.Errorf("load alert states: %w", err)
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (state, error) {
		var st state
		err := r.Scan(&st.TargetType, &st.TargetID, &st.Scope, &st.TenantID, &st.TargetName, &st.Status,
			&st.Severity, &st.Summary, &st.PendingSince, &st.FiringSince, &st.LastNotifiedAt)
		return st, err
	})
	if err != nil {
		return nil, fmt.Errorf("load alert states: %w", err)
	}
	out := make(map[string]state, len(list))
	for _, st := range list {
		out[st.key()] = st
	}
	return out, nil
}

// leftScope reports why a resolved target no longer counts, or "" when its
// condition simply cleared: its tenant was suspended, or the relay, pool,
// connector or shield was removed.
func leftScope(ctx context.Context, tx pgx.Tx, st state) (string, error) {
	var reason string
	if st.TenantID != nil {
		var suspended bool
		err := tx.QueryRow(ctx, `SELECT suspended_at IS NOT NULL FROM workspaces WHERE id = $1::uuid`, *st.TenantID).Scan(&suspended)
		if errors.Is(err, pgx.ErrNoRows) {
			return "tenant removed", nil
		}
		if err != nil {
			return "", fmt.Errorf("resolve reason: %w", err)
		}
		if suspended {
			return "tenant suspended", nil
		}
	}
	var q string
	switch st.TargetType {
	case "relay":
		q = `SELECT CASE WHEN status IN ('revoked', 'deleted') THEN 'relay removed' ELSE '' END FROM relays WHERE id::text = $1`
	case "pool":
		q = `SELECT '' FROM relay_pools WHERE id::text = $1`
	case "connector":
		q = `SELECT CASE WHEN status = 'revoked' OR revoked_at IS NOT NULL THEN 'connector removed' ELSE '' END FROM connectors WHERE id::text = $1`
	case "shield":
		q = `SELECT CASE WHEN status = 'revoked' THEN 'shield removed' ELSE '' END FROM shields WHERE id::text = $1`
	default:
		return "", nil
	}
	err := tx.QueryRow(ctx, q, st.TargetID).Scan(&reason)
	if errors.Is(err, pgx.ErrNoRows) {
		return st.TargetType + " removed", nil
	}
	if err != nil {
		return "", fmt.Errorf("resolve reason: %w", err)
	}
	return reason, nil
}

func insertEvent(ctx context.Context, tx pgx.Tx, r Rule, st state, transition, summary string, now time.Time) (Event, error) {
	status := statusFiring
	if transition == TransitionResolved {
		status = "resolved"
	}
	started := st.PendingSince
	if st.FiringSince != nil {
		started = *st.FiringSince
	}
	ev := Event{
		RuleID: r.ID, Kind: r.Kind, Scope: st.Scope, TenantID: st.TenantID,
		TargetType: st.TargetType, TargetID: st.TargetID, TargetName: st.TargetName,
		Transition: transition, Status: status, Severity: st.Severity, Summary: summary,
		StartedAt: started.UTC(), OccurredAt: now.UTC(),
	}
	err := tx.QueryRow(ctx, `
INSERT INTO alert_events (rule_id, kind, scope, tenant_id, target_type, target_id, target_name,
                          transition, status, severity, summary, started_at, occurred_at)
VALUES ($1, $2, $3, $4::uuid, $5, $6, $7, $8, $9, $10, $11, $12, $13) RETURNING id::text`,
		ev.RuleID, ev.Kind, ev.Scope, ev.TenantID, ev.TargetType, ev.TargetID, ev.TargetName,
		ev.Transition, ev.Status, ev.Severity, ev.Summary, ev.StartedAt, ev.OccurredAt).Scan(&ev.ID)
	if err != nil {
		return Event{}, fmt.Errorf("insert alert event: %w", err)
	}
	// A2 (D-52): route the event to every enabled channel whose scope filter
	// and minimum severity match, in this transaction, so a committed event
	// always has its deliveries queued.
	if _, err := tx.Exec(ctx, `
INSERT INTO alert_delivery_queue (event_id, channel_id)
SELECT $1::uuid, c.id FROM alert_channels c
 WHERE c.enabled
   AND (c.scope_filter = 'all' OR c.scope_filter = $2)
   AND `+severityRank("$3")+` <= `+severityRank("c.min_severity"),
		ev.ID, ev.Scope, ev.Severity); err != nil {
		return Event{}, fmt.Errorf("queue alert deliveries: %w", err)
	}
	return ev, nil
}

// severityRank is a SQL expression ranking a severity (critical 0, warning
// 1, info 2): an event matches a channel when its rank is at most the
// channel's minimum.
func severityRank(expr string) string {
	return `(CASE ` + expr + ` WHEN 'critical' THEN 0 WHEN 'warning' THEN 1 ELSE 2 END)`
}

// Apply moves one rule's states to match obs (the targets whose condition
// holds now) and returns the events it recorded, all in one transaction:
//
//	condition holds, no state          → pending (fires at once when for_seconds is 0)
//	pending for at least for_seconds   → firing, event "fired"
//	firing, renotify due               → event "renotified"
//	condition no longer holds: pending → removed (no event); firing → event "resolved", removed
//
// A disabled rule passes obs = nil, which resolves everything it has firing.
// The caller must not call Apply when the condition could not be read.
func (s *Store) Apply(ctx context.Context, r Rule, obs []Observation, now time.Time, resolvedSummary string) ([]Event, error) {
	var events []Event
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		// The rule may have been deleted or changed since it was listed;
		// lock it so a concurrent delete can't interleave.
		cur, err := scanRule(tx.QueryRow(ctx, `SELECT `+ruleColumns+` FROM alert_rules WHERE id = $1 FOR UPDATE`, r.ID))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		// The rule changed since it was listed (a PATCH landed while its
		// condition was being read): obs was computed for the old version.
		// Change nothing; the next tick evaluates the current rule.
		if !cur.UpdatedAt.Equal(r.UpdatedAt) || cur.Enabled != r.Enabled {
			return nil
		}
		if !cur.Enabled {
			obs = nil
		}
		states, err := loadStates(ctx, tx, cur.ID)
		if err != nil {
			return err
		}
		seen := make(map[string]bool, len(obs))
		for _, o := range obs {
			k := o.TargetType + "\x00" + o.TargetID
			if seen[k] {
				continue
			}
			seen[k] = true
			st, exists := states[k]
			var tenant *string
			if o.TenantID != "" {
				t := o.TenantID
				tenant = &t
			}
			if !exists {
				st = state{TargetType: o.TargetType, TargetID: o.TargetID, Scope: cur.Scope, TenantID: tenant,
					Status: statusPending, PendingSince: now}
			}
			st.TargetName, st.Summary, st.Severity = o.TargetName, o.Summary, cur.Severity
			transition := ""
			switch {
			case st.Status == statusPending && now.Sub(st.PendingSince) >= time.Duration(cur.ForSeconds)*time.Second:
				st.Status = statusFiring
				f := now
				st.FiringSince, st.LastNotifiedAt = &f, &f
				transition = TransitionFired
			case st.Status == statusFiring && cur.RenotifySeconds > 0 && st.LastNotifiedAt != nil &&
				now.Sub(*st.LastNotifiedAt) >= time.Duration(cur.RenotifySeconds)*time.Second:
				f := now
				st.LastNotifiedAt = &f
				transition = TransitionRenotified
			}
			if _, err := tx.Exec(ctx, `
INSERT INTO alert_states (rule_id, target_type, target_id, scope, tenant_id, target_name, status, severity,
                          summary, pending_since, firing_since, last_notified_at)
VALUES ($1, $2, $3, $4, $5::uuid, $6, $7, $8, $9, $10, $11, $12)
ON CONFLICT (rule_id, target_type, target_id) DO UPDATE
   SET target_name = EXCLUDED.target_name, status = EXCLUDED.status, severity = EXCLUDED.severity,
       summary = EXCLUDED.summary, firing_since = EXCLUDED.firing_since,
       last_notified_at = EXCLUDED.last_notified_at`,
				cur.ID, st.TargetType, st.TargetID, st.Scope, st.TenantID, st.TargetName, st.Status, st.Severity,
				st.Summary, st.PendingSince, st.FiringSince, st.LastNotifiedAt); err != nil {
				return fmt.Errorf("write alert state: %w", err)
			}
			if transition != "" {
				ev, err := insertEvent(ctx, tx, cur, st, transition, st.Summary, now)
				if err != nil {
					return err
				}
				events = append(events, ev)
			}
		}
		for k, st := range states {
			if seen[k] {
				continue
			}
			if st.Status == statusFiring {
				summary := "Resolved: " + st.Summary
				reason := resolvedSummary
				if !cur.Enabled {
					reason = "rule disabled"
				} else if reason == "" {
					// A target that left the rule's scope isn't "fixed":
					// say why it no longer counts.
					if reason, err = leftScope(ctx, tx, st); err != nil {
						return err
					}
				}
				if reason != "" {
					summary = "Resolved (" + reason + "): " + st.Summary
				}
				ev, err := insertEvent(ctx, tx, cur, st, TransitionResolved, summary, now)
				if err != nil {
					return err
				}
				events = append(events, ev)
			}
			if _, err := tx.Exec(ctx, `DELETE FROM alert_states WHERE rule_id = $1 AND target_type = $2 AND target_id = $3`,
				cur.ID, st.TargetType, st.TargetID); err != nil {
				return fmt.Errorf("clear alert state: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return events, nil
}
