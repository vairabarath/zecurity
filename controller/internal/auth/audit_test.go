package auth

import (
	"context"
	"testing"

	"github.com/yourorg/ztna/controller/internal/audit"
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
