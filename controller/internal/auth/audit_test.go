package auth

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/yourorg/ztna/controller/internal/audit"
	"github.com/yourorg/ztna/controller/internal/identity"
)

// PENDING-11 P-9 / S-11: sign-out is audited once per session actually
// ended, with the channel flag; replaying the token adds nothing.
func TestEndSession_AuditsSignOutOnce(t *testing.T) {
	rc, _ := newTestValkey(t)
	var got []audit.Entry
	cfg := testConfig()
	cfg.Audit = func(_ context.Context, e audit.Entry) { got = append(got, e) }
	svc := &serviceImpl{cfg: cfg, redisClient: rc}
	ctx := context.Background()

	consoleTok, _, _, _ := svc.IssueSession(ctx, "user-1", "tenant-1", "admin", "u@example.com", 1)
	clientTok, _, _, _ := svc.IssueSession(ctx, "user-1", "tenant-1", "admin", "u@example.com", 1)

	svc.EndSession(ctx, consoleTok)
	svc.EndSession(ctx, consoleTok) // replay: session already gone
	svc.EndSession(WithClientChannel(ctx), clientTok)
	svc.EndSession(ctx, "not-a-jwt")

	if len(got) != 2 {
		t.Fatalf("got %d audit rows, want 2: %+v", len(got), got)
	}
	for i, wantClient := range []bool{false, true} {
		e := got[i]
		if e.Action != ActionSignOut || e.TenantID != "tenant-1" || e.ActorUserID != "user-1" ||
			e.ActorEmail != "u@example.com" || e.Details["client"] != wantClient {
			t.Fatalf("row %d = %+v, want sign_out client=%v", i, e, wantClient)
		}
	}
}

// PENDING-11 note §7: a refused sign-in is audited only on the workspace's own
// connection and only for "not permitted".
func TestAuditSignInFailure(t *testing.T) {
	ws, other := "ws-1", "ws-2"
	clientNotInvited := errors.New("client: not invited")
	notActive := fmt.Errorf("lifecycle: %w", identity.ErrUserNotActive)
	for _, tc := range []struct {
		name string
		conn *string
		err  error
		want bool
	}{
		{"platform connection", nil, identity.ErrNotInvited, false},
		{"another workspace's connection", &other, identity.ErrNotInvited, false},
		{"own connection, not invited", &ws, identity.ErrNotInvited, true},
		{"own connection, not active (wrapped)", &ws, notActive, true},
		{"own connection, caller's not-invited error", &ws, clientNotInvited, true},
		{"own connection, other error", &ws, errors.New("db down"), false},
		{"own connection, no error", &ws, nil, false},
	} {
		if got := AuditSignInFailure(tc.conn, ws, tc.err, clientNotInvited); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}
