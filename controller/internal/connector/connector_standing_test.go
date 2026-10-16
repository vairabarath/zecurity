package connector

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	pb "github.com/yourorg/ztna/controller/gen/go/proto/connector/v1"
	"github.com/yourorg/ztna/controller/internal/spiffe"
)

// Certificate audit bug 7: the interceptor's connector standing comes from
// the database, scoped to the certificate's trust domain.
func TestLoadConnectorStanding(t *testing.T) {
	pool, ctx := setupRevocationTestDB(t)
	slug := fmt.Sprintf("standing-%d", time.Now().UnixNano())
	wsID, td, rnID := seedRevocationWorkspace(t, pool, ctx, slug)
	now := time.Now()
	active := seedConnector(t, pool, ctx, wsID, rnID, td, "active", nil)
	revoked := seedConnector(t, pool, ctx, wsID, rnID, td, "revoked", &now)
	legacy := seedConnector(t, pool, ctx, wsID, rnID, td, "active", &now)

	for name, tc := range map[string]struct {
		id, td  string
		found   bool
		revoked bool
	}{
		"active":                        {active, td, true, false},
		"revoked":                       {revoked, td, true, true},
		"revoked_at with status active": {legacy, td, true, true},
		"other trust domain":            {active, "other.zecurity.in", false, false},
		"unknown connector":             {uuid.NewString(), td, false, false},
		"entity id is not a UUID":       {"not-a-uuid", td, false, false},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := LoadConnectorStanding(ctx, pool, tc.td, tc.id)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if (got != nil) != tc.found {
				t.Fatalf("found = %v, want %v", got != nil, tc.found)
			}
			if got != nil && got.Revoked() != tc.revoked {
				t.Fatalf("revoked = %v, want %v (%+v)", got.Revoked(), tc.revoked, got)
			}
		})
	}

	// Unreadable state is an error, never "not found".
	closed, _ := setupRevocationTestDB(t)
	closed.Close()
	if got, err := LoadConnectorStanding(ctx, closed, td, active); err == nil || got != nil {
		t.Fatalf("closed pool: got %+v, err %v; want an error", got, err)
	}
}

// Goodbye from a connector carrying revoked_at must not change its row, even
// when its status column was left 'active'. (The interceptor refuses such a
// connector first; the handler is defence in depth.)
func TestGoodbye_RevokedAtWithActiveStatusLeavesRowUntouched(t *testing.T) {
	pool, ctx := setupRevocationTestDB(t)
	slug := fmt.Sprintf("gb-legacy-%d", time.Now().UnixNano())
	wsID, td, rnID := seedRevocationWorkspace(t, pool, ctx, slug)
	now := time.Now()
	connID := seedConnector(t, pool, ctx, wsID, rnID, td, "active", &now)

	policyStub := &recordingPolicyNotifier{}
	transportStub := &recordingTransportNotifier{}
	h := &EnrollmentHandler{Pool: pool, PolicyNotifier: policyStub, TransportNotifier: transportStub}
	reqCtx := spiffe.WithIdentity(ctx, fmt.Sprintf("spiffe://%s/connector/%s", td, connID), "connector", connID, td)

	if _, err := h.Goodbye(reqCtx, &pb.GoodbyeRequest{}); err != nil {
		t.Fatalf("Goodbye error: %v", err)
	}
	var st string
	var revokedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT status, revoked_at FROM connectors WHERE id = $1`, connID).Scan(&st, &revokedAt); err != nil {
		t.Fatalf("query connector: %v", err)
	}
	if st != "active" || revokedAt == nil {
		t.Fatalf("status=%q revoked_at=%v: Goodbye altered a revoked connector", st, revokedAt)
	}
	if policyStub.calls != 0 || len(transportStub.calls) != 0 {
		t.Fatalf("notifiers called for a revoked connector: policy=%d transport=%v", policyStub.calls, transportStub.calls)
	}
}
