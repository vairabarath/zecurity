// Package audit writes durable records of privileged / break-glass actions to
// the audit_logs table. It exists so that operations which bypass a safety
// invariant (e.g. forceDeleteResource, ADR-004 Phase 4) leave an immutable,
// queryable trail of who did what to which target and when.
package audit

import (
	"context"
	"encoding/json"
	"log"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Entry is one privileged-action audit record.
type Entry struct {
	TenantID    string         // workspace UUID
	ActorUserID string         // user UUID; "" → stored NULL (system/automated action)
	ActorEmail  string         // authenticated user's email
	Action      string         // dotted verb, e.g. "resource.force_delete"
	TargetType  string         // e.g. "resource"
	TargetID    string         // id of the affected object
	Details     map[string]any // context snapshot; nil → NULL
}

// Record writes a durable audit entry.
//
// Audit writes are best-effort relative to the action they describe: by the time
// Record is called the privileged action has ALREADY happened, so a write failure
// must not be mistaken for "the action didn't occur". Record therefore logs any
// failure loudly and also returns it, leaving the caller to decide — the expected
// pattern for a completed-and-authoritative action is log-and-continue, not abort.
func Record(ctx context.Context, db *pgxpool.Pool, e Entry) error {
	var details []byte
	if e.Details != nil {
		b, err := json.Marshal(e.Details)
		if err != nil {
			// Don't lose the whole record over a bad detail blob — store NULL details.
			log.Printf("audit: marshal details for %s on %s/%s: %v", e.Action, e.TargetType, e.TargetID, err)
		} else {
			details = b
		}
	}

	var actorUserID any // leave NULL when there is no user (system action)
	if e.ActorUserID != "" {
		actorUserID = e.ActorUserID
	}

	_, err := db.Exec(ctx,
		`INSERT INTO audit_logs
		   (tenant_id, actor_user_id, actor_email, action, target_type, target_id, details)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		e.TenantID, actorUserID, e.ActorEmail, e.Action, e.TargetType, e.TargetID, details,
	)
	if err != nil {
		log.Printf("audit: FAILED to record %s on %s/%s by %s: %v",
			e.Action, e.TargetType, e.TargetID, e.ActorEmail, err)
		return err
	}

	log.Printf("audit: %s on %s/%s by %s", e.Action, e.TargetType, e.TargetID, e.ActorEmail)
	return nil
}
