// Package auditexport ships a workspace's hash-chained audit streams
// (internal/audit, migration 053) to external SIEMs (PENDING-11 P-5 … P-8).
//
// One exporter inside the controller (D-02) tails each stream by its gap-free
// seq, per sink cursor, and hands batches to an Adapter (webhook, Splunk
// HEC). Delivery is at-least-once: the cursor advances only after the
// destination acknowledges, and every event carries (workspace_id, stream,
// seq) so the SIEM can deduplicate.
package auditexport

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"github.com/yourorg/ztna/controller/internal/audit"
)

// Schema names the event format. Fields are only ever added, never changed.
const Schema = "zecurity.audit.v1"

// Synthetic (non-chained) event actions.
const (
	ActionGap  = "export.gap"  // rows purged by retention before this sink got them
	ActionTest = "export.test" // testAuditExportSink
)

// Actor is who did it.
type Actor struct {
	Type   string  `json:"type"` // "user" or "system"
	UserID *string `json:"user_id,omitempty"`
	Email  string  `json:"email,omitempty"`
}

// Target is what it was done to.
type Target struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// Subject is the client identity of an access event.
type Subject struct {
	UserID   *string `json:"user_id,omitempty"`
	DeviceID *string `json:"device_id,omitempty"`
	SpiffeID *string `json:"spiffe_id,omitempty"`
}

// Event is one exported zecurity.audit.v1 event (P-7). For a chained event,
// row_hash = sha256(prev_hash || base64-decoded canonical).
type Event struct {
	WorkspaceID string   `json:"workspace_id"`
	Stream      string   `json:"stream"`
	Seq         int64    `json:"seq,omitempty"`
	PrevHash    string   `json:"prev_hash,omitempty"` // hex
	RowHash     string   `json:"row_hash,omitempty"`  // hex
	OccurredAt  string   `json:"occurred_at"`
	RecordedAt  string   `json:"recorded_at"`
	Action      string   `json:"action"`
	Outcome     string   `json:"outcome"`
	Actor       *Actor   `json:"actor,omitempty"`
	Target      *Target  `json:"target,omitempty"`
	Subject     *Subject `json:"subject,omitempty"`
	Details     any      `json:"details"`
	// DetailsCanonical is the exact jsonb text that was hashed (admin stream);
	// a JSON parser can't reproduce it from Details.
	DetailsCanonical *string `json:"details_canonical,omitempty"`
	// Canonical is the whole hashed byte string, base64.
	Canonical string `json:"canonical,omitempty"`
}

func ts(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000000Z07:00") }

// AdminEvent maps an audit_logs row.
func AdminEvent(r audit.AdminRow) Event {
	actorType := "system"
	if r.ActorUserID != nil || strings.HasPrefix(r.Action, "auth.") {
		actorType = "user"
	}
	outcome := "success"
	if strings.HasSuffix(r.Action, "_failed") || strings.HasSuffix(r.Action, "_denied") {
		outcome = "failure"
	}
	var details any
	if r.Details != nil {
		details = json.RawMessage(*r.Details)
	}
	return Event{
		WorkspaceID:      r.WorkspaceID,
		Stream:           string(audit.StreamAdmin),
		Seq:              r.Seq,
		PrevHash:         hex.EncodeToString(r.PrevHash),
		RowHash:          hex.EncodeToString(r.RowHash),
		OccurredAt:       ts(r.CreatedAt),
		RecordedAt:       ts(r.CreatedAt),
		Action:           r.Action,
		Outcome:          outcome,
		Actor:            &Actor{Type: actorType, UserID: r.ActorUserID, Email: r.ActorEmail},
		Target:           &Target{Type: r.TargetType, ID: r.TargetID},
		Details:          details,
		DetailsCanonical: r.Details,
		Canonical:        base64.StdEncoding.EncodeToString(r.Canonical()),
	}
}

// AccessEvent maps a connector_logs row: action "resource.access", outcome
// allow / deny / error.
func AccessEvent(r audit.AccessRow) Event {
	occurred := r.CreatedAt
	if r.OccurredAt != nil {
		occurred = *r.OccurredAt
	}
	outcome := "unknown"
	if r.Action != nil {
		outcome = *r.Action
	}
	var target *Target
	if r.ResourceID != nil {
		target = &Target{Type: "resource", ID: *r.ResourceID}
	}
	return Event{
		WorkspaceID: r.WorkspaceID,
		Stream:      string(audit.StreamAccess),
		Seq:         r.Seq,
		PrevHash:    hex.EncodeToString(r.PrevHash),
		RowHash:     hex.EncodeToString(r.RowHash),
		OccurredAt:  ts(occurred),
		RecordedAt:  ts(r.CreatedAt),
		Action:      "resource.access",
		Outcome:     outcome,
		Actor:       &Actor{Type: "user", UserID: r.UserID},
		Target:      target,
		Subject:     &Subject{UserID: r.UserID, DeviceID: r.ClientDeviceID, SpiffeID: r.ClientSpiffeID},
		Details: map[string]any{
			"resource_id":  r.ResourceID,
			"destination":  r.Destination,
			"port":         r.Port,
			"protocol":     r.Protocol,
			"route_type":   r.RouteType,
			"connector_id": r.ConnectorID,
			"error":        r.Error,
			"message":      r.Message,
		},
		Canonical: base64.StdEncoding.EncodeToString(r.Canonical()),
	}
}

// GapEvent says rows fromSeq..toSeq were purged by retention before this sink
// received them; the chain continues from checkpointHash (the row_hash of
// toSeq). A SIEM can tell this retention gap from tampering.
func GapEvent(workspaceID string, s audit.Stream, fromSeq, toSeq int64, checkpointHash []byte, now time.Time) Event {
	return Event{
		WorkspaceID: workspaceID,
		Stream:      string(s),
		OccurredAt:  ts(now),
		RecordedAt:  ts(now),
		Action:      ActionGap,
		Outcome:     "failure",
		Actor:       &Actor{Type: "system"},
		Details: map[string]any{
			"from_seq":        fromSeq,
			"to_seq":          toSeq,
			"checkpoint_hash": hex.EncodeToString(checkpointHash),
		},
	}
}

// TestEvent is the signed probe sent by testAuditExportSink.
func TestEvent(workspaceID, sinkID string, now time.Time) Event {
	return Event{
		WorkspaceID: workspaceID,
		Stream:      string(audit.StreamAdmin),
		OccurredAt:  ts(now),
		RecordedAt:  ts(now),
		Action:      ActionTest,
		Outcome:     "success",
		Actor:       &Actor{Type: "system"},
		Target:      &Target{Type: "audit_export_sink", ID: sinkID},
		Details:     map[string]any{},
	}
}
