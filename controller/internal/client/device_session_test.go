package client

import (
	"context"
	"testing"
	"time"

	clientv1 "github.com/yourorg/ztna/controller/gen/go/proto/client/v1"
	"github.com/yourorg/ztna/controller/internal/auth"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// noSessionAuth returns its claims unchanged, so a token without a sid stays
// without one.
type noSessionAuth struct{ postureTestAuth }

func (a *noSessionAuth) VerifyAccessToken(string) (*auth.AccessTokenClaims, error) {
	return a.claims, nil
}

// endedSessionAuth reports every session as ended.
type endedSessionAuth struct{ postureTestAuth }

func (*endedSessionAuth) SessionDeadlines(context.Context, string, string) (auth.SessionDeadlines, error) {
	return auth.SessionDeadlines{}, auth.ErrSessionNotFound
}

// PENDING-09 D-1: an enrolled device records the session it was enrolled
// under and that session's deadlines.
func TestEnrollDevice_BindsDeviceToSession(t *testing.T) {
	f := newDeviceHistoryFixture(t)
	deviceID, _, _ := f.enrolled("bound")

	var sid string
	var idle, max time.Time
	if err := f.pool.QueryRow(f.ctx,
		`SELECT session_id, session_idle_until, session_max_until FROM client_devices WHERE id = $1`,
		deviceID).Scan(&sid, &idle, &max); err != nil {
		t.Fatal(err)
	}
	if sid != "test-sid" {
		t.Fatalf("session_id %q, want test-sid", sid)
	}
	if d := time.Until(idle); d < 6*24*time.Hour || d > 8*24*time.Hour {
		t.Fatalf("idle deadline in %v", d)
	}
	if d := time.Until(max); d < 29*24*time.Hour || d > 31*24*time.Hour {
		t.Fatalf("max deadline in %v", d)
	}
}

// A token without a session, or whose session has ended, cannot enroll a
// device: it could never be revoked when its session ends.
func TestEnrollDevice_RefusesWithoutLiveSession(t *testing.T) {
	f := newDeviceHistoryFixture(t)
	claims := &auth.AccessTokenClaims{UserID: f.userID, TenantID: f.wsID, Email: "history@example.test", Generation: 1}
	for name, a := range map[string]auth.Service{
		"no sid":        &noSessionAuth{postureTestAuth{claims: claims}},
		"ended session": &endedSessionAuth{postureTestAuth{claims: claims}},
	} {
		f.svc.authSvc = a
		csrPEM, _ := makeClientCSR(t)
		_, err := f.svc.EnrollDevice(f.ctx, &clientv1.EnrollDeviceRequest{
			AccessToken: "token", CsrPem: csrPEM, DeviceName: "refused", Os: "linux",
		})
		if status.Code(err) != codes.Unauthenticated {
			t.Fatalf("%s: err %v, want Unauthenticated", name, err)
		}
	}
	var n int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM client_devices WHERE name = 'refused'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("refused enrollments left %d device rows (%v)", n, err)
	}
}
