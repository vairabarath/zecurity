package identity

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Identity event action verbs (dotted, stable). Callers publish these as
// Events, never raw log rows, so a real event bus (SOC/metrics/notifications)
// can replace the audit sink later with no call-site change.
const (
	ActionUserProvisioned = "identity.user.provisioned" // first login → new canonical user
	ActionIdentityLinked  = "identity.link"             // external identity linked to a user
	ActionGenerationBump  = "session.generation.bump"   // mass session revocation for a user
)

// Event is a single identity-plane occurrence. It is tenant-scoped: the sink is
// audit_logs, whose tenant_id/actor_email are NOT NULL, so events without a
// resolved workspace (e.g. a pre-tenant login failure) are not published here.
type Event struct {
	TenantID    string
	ActorUserID string // "" when the acting principal has no user row yet
	ActorEmail  string
	Action      string
	TargetType  string
	TargetID    string
	Details     map[string]any
}

// EventPublisher is the identity-plane event boundary. The only implementation
// today writes audit_logs; the interface exists so a bus can drop in later.
type EventPublisher interface {
	Publish(ctx context.Context, e Event) error
}

// auditSink appends events to audit_logs (migration 016) — the sole sink today.
type auditSink struct{ pool *pgxpool.Pool }

// NewAuditSink returns an EventPublisher backed by the append-only audit_logs
// table. audit_logs is tenant-scoped and append-only by convention.
func NewAuditSink(pool *pgxpool.Pool) EventPublisher { return &auditSink{pool: pool} }

func (a *auditSink) Publish(ctx context.Context, e Event) error {
	var details []byte
	if e.Details != nil {
		b, err := json.Marshal(e.Details)
		if err != nil {
			return fmt.Errorf("marshal event details: %w", err)
		}
		details = b
	}
	// actor_user_id is nullable (system actions have no user); the rest are NOT NULL.
	var actor *string
	if e.ActorUserID != "" {
		actor = &e.ActorUserID
	}
	_, err := a.pool.Exec(ctx,
		`INSERT INTO audit_logs
		   (tenant_id, actor_user_id, actor_email, action, target_type, target_id, details)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		e.TenantID, actor, e.ActorEmail, e.Action, e.TargetType, e.TargetID, details)
	if err != nil {
		return fmt.Errorf("insert audit_log: %w", err)
	}
	return nil
}

// NopPublisher discards events. Used where auditing is intentionally absent
// (tests, or call sites that have no tenant to attribute the event to).
type NopPublisher struct{}

func (NopPublisher) Publish(context.Context, Event) error { return nil }
