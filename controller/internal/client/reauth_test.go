package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	clientv1 "github.com/yourorg/ztna/controller/gen/go/proto/client/v1"
	"github.com/yourorg/ztna/controller/internal/auth/providers"
	"github.com/yourorg/ztna/controller/internal/mfa"
	"github.com/yourorg/ztna/controller/internal/policy"
	"github.com/yourorg/ztna/controller/internal/risk"
	"github.com/yourorg/ztna/controller/internal/totp"
)

// memLimiter is an in-memory mfa.Limiter.
type memLimiter struct {
	mu sync.Mutex
	n  map[string]int
}

func (l *memLimiter) Blocked(_ context.Context, u string) (bool, time.Duration, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.n[u] >= mfa.FailMax, time.Minute, nil
}

func (l *memLimiter) RecordFailure(_ context.Context, u string) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.n[u]++
	return l.n[u] == mfa.FailMax, nil
}

func (l *memLimiter) Reset(_ context.Context, u string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.n, u)
	return nil
}

type stepUpFixture struct {
	*deviceHistoryFixture
	deviceID string
	mfaSvc   *mfa.Service
}

func newStepUpFixture(t *testing.T) *stepUpFixture {
	t.Helper()
	f := newDeviceHistoryFixture(t)
	mfaSvc := mfa.NewService(mfa.NewStore(f.pool), f.pki, &memLimiter{n: map[string]int{}})
	f.svc.policyStore = policy.NewStore(f.pool)
	f.svc.EnableStepUp(mfaSvc, risk.NewService(f.pool, nil))
	deviceID, _, _ := f.enrolled("step-up")
	return &stepUpFixture{deviceHistoryFixture: f, deviceID: deviceID, mfaSvc: mfaSvc}
}

type deviceAuthRow struct {
	authAt, mfaAt, reauthAt *time.Time
	score                   int
	reasons                 []string
}

func (f *stepUpFixture) device() deviceAuthRow {
	f.t.Helper()
	var d deviceAuthRow
	if err := f.pool.QueryRow(f.ctx,
		`SELECT session_auth_at, session_mfa_at, reauth_required_at, risk_score, risk_reasons
		   FROM client_devices WHERE id = $1`, f.deviceID,
	).Scan(&d.authAt, &d.mfaAt, &d.reauthAt, &d.score, &d.reasons); err != nil {
		f.t.Fatal(err)
	}
	return d
}

func (f *stepUpFixture) audits(action string) int {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(f.ctx,
		`SELECT count(*) FROM audit_logs WHERE tenant_id = $1 AND action = $2`, f.wsID, action).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func (f *stepUpFixture) reauth() reauthInfo {
	return reauthInfo{UserID: f.userID, DeviceID: f.deviceID, SID: "test-sid", Email: "history@example.test"}
}

// ticket simulates the state right after the IdP round trip.
func (f *stepUpFixture) ticket(skippable, enrol bool) string {
	f.t.Helper()
	tk := &mfaTicket{WorkspaceID: f.wsID, Reauth: f.reauth(), Skippable: skippable, ExpiresAt: time.Now().Add(time.Minute)}
	if enrol {
		enr, err := f.mfaSvc.StartEnrollment(f.ctx, f.userID, f.wsID, "history@example.test")
		if err != nil {
			f.t.Fatalf("start enrolment: %v", err)
		}
		tk.Enrollment = enr
	}
	id, _ := newCtrlCode()
	putMFATicket(id, tk)
	return id
}

func (f *stepUpFixture) post(form url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/clients/reauth/mfa", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	f.svc.ReauthMFAHandler().ServeHTTP(rec, req)
	return rec
}

// PENDING-06: a device starts with its session's authentication time and no MFA.
func TestEnrollDevice_RecordsSessionAuthentication(t *testing.T) {
	f := newStepUpFixture(t)
	d := f.device()
	if d.authAt == nil || time.Since(*d.authAt) > time.Minute {
		t.Fatalf("session_auth_at = %v, want about now", d.authAt)
	}
	if d.mfaAt != nil {
		t.Fatalf("session_mfa_at = %v, want NULL (the session never passed MFA)", d.mfaAt)
	}
}

// M-5: the TOTP page enrols on first use, refuses wrong codes, stamps the
// device on the right one and is single-use.
func TestReauthMFA_EnrolVerifyStamp(t *testing.T) {
	f := newStepUpFixture(t)
	if _, err := f.pool.Exec(f.ctx,
		`UPDATE client_devices SET session_auth_at = NOW() - INTERVAL '3 hours', risk_score = 60,
		        risk_reasons = '{country_changed}', reauth_required_at = NOW() - INTERVAL '1 minute'
		  WHERE id = $1`, f.deviceID); err != nil {
		t.Fatal(err)
	}
	id := f.ticket(true, true)
	tk, _ := getMFATicket(id)
	secret, _ := totp.DecodeSecret(tk.Enrollment.Secret)

	// The enrolment page shows the key and a QR code, and is never cached.
	rec := httptest.NewRecorder()
	stepUpHeaders(rec)
	renderStepUp(rec, http.StatusOK, mfaPage(id, tk, ""))
	body := rec.Body.String()
	if !strings.Contains(body, tk.Enrollment.Secret) || !strings.Contains(body, "data:image/png;base64,") {
		t.Fatal("enrolment page lacks the key or the QR code")
	}
	if rec.Header().Get("Cache-Control") != "no-store" || !strings.Contains(rec.Header().Get("Content-Security-Policy"), "default-src 'none'") {
		t.Fatalf("enrolment page headers: %v", rec.Header())
	}

	// Wrong code: page again with an error, nothing stamped.
	rec = f.post(url.Values{"ticket": {id}, "code": {"000000"}, "action": {"verify"}})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "not valid") {
		t.Fatalf("wrong code: %d %s", rec.Code, rec.Body.String())
	}
	if d := f.device(); d.mfaAt != nil || d.reauthAt == nil {
		t.Fatalf("a wrong code changed the device: %+v", d)
	}

	// Right code: authenticated, MFA stamped, risk cleared, audited.
	rec = f.post(url.Values{"ticket": {id}, "code": {totp.Code(secret, time.Now())}, "action": {"verify"}})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "You&#39;re authenticated") {
		t.Fatalf("right code: %d %s", rec.Code, rec.Body.String())
	}
	d := f.device()
	if d.authAt == nil || time.Since(*d.authAt) > time.Minute || d.mfaAt == nil || time.Since(*d.mfaAt) > time.Minute {
		t.Fatalf("device not stamped: %+v", d)
	}
	if d.reauthAt != nil || d.score != 0 || len(d.reasons) != 0 {
		t.Fatalf("risk not cleared: %+v", d)
	}
	if f.audits(ActionStepUp) != 1 || f.audits(ActionMFAEnrolled) != 1 {
		t.Fatalf("audit rows: step_up=%d enrolled=%d, want 1 and 1", f.audits(ActionStepUp), f.audits(ActionMFAEnrolled))
	}
	if ok, _ := f.mfaSvc.Enrolled(f.ctx, f.userID); !ok {
		t.Fatal("authenticator not confirmed")
	}

	// The ticket is single-use.
	rec = f.post(url.Values{"ticket": {id}, "code": {"000000"}, "action": {"verify"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("reused ticket: %d, want 400", rec.Code)
	}
	// The device is now waiting for nothing.
	st, err := f.svc.policyStore.GetDeviceAuthState(f.ctx, f.wsID, f.deviceID)
	if err != nil || st.Pending(time.Now()) != 0 {
		t.Fatalf("pending after step-up: %v err=%v", st, err)
	}
}

// Skip stamps the authentication only, and only when the ticket allows it.
func TestReauthMFA_Skip(t *testing.T) {
	f := newStepUpFixture(t)
	if _, err := f.pool.Exec(f.ctx, `UPDATE client_devices SET session_auth_at = NOW() - INTERVAL '3 hours' WHERE id = $1`, f.deviceID); err != nil {
		t.Fatal(err)
	}
	mandatory := f.ticket(false, true)
	rec := f.post(url.Values{"ticket": {mandatory}, "action": {"skip"}})
	if !strings.Contains(rec.Body.String(), "A code is required") {
		t.Fatalf("skip on a mandatory ticket: %s", rec.Body.String())
	}
	if d := f.device(); time.Since(*d.authAt) < time.Hour {
		t.Fatal("a refused skip stamped the device")
	}

	optional := f.ticket(true, true)
	rec = f.post(url.Values{"ticket": {optional}, "action": {"skip"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("skip: %d", rec.Code)
	}
	d := f.device()
	if time.Since(*d.authAt) > time.Minute || d.mfaAt != nil {
		t.Fatalf("after skip: %+v, want a fresh authentication and no MFA", d)
	}
	if ok, _ := f.mfaSvc.Enrolled(f.ctx, f.userID); ok {
		t.Fatal("skipping confirmed an authenticator")
	}
}

// A step-up only stamps the device's own live session.
func TestCompleteStepUp_OnlyTheDevicesSession(t *testing.T) {
	f := newStepUpFixture(t)
	if _, err := f.pool.Exec(f.ctx, `UPDATE client_devices SET session_auth_at = NOW() - INTERVAL '3 hours' WHERE id = $1`, f.deviceID); err != nil {
		t.Fatal(err)
	}
	wrongSID := f.reauth()
	wrongSID.SID = "another-session"
	if f.svc.completeStepUp(f.ctx, f.wsID, &wrongSID, true, "totp") {
		t.Fatal("a step-up for another session stamped the device")
	}
	ra := f.reauth()
	if _, err := f.pool.Exec(f.ctx, `UPDATE client_devices SET revoked_at = NOW() WHERE id = $1`, f.deviceID); err != nil {
		t.Fatal(err)
	}
	if f.svc.completeStepUp(f.ctx, f.wsID, &ra, true, "totp") {
		t.Fatal("a step-up stamped a revoked device")
	}
	if d := f.device(); time.Since(*d.authAt) < time.Hour || d.mfaAt != nil {
		t.Fatalf("device changed: %+v", d)
	}
}

// The IdP must return the user the device belongs to.
func TestFinishReauthIdP_IdentityMustMatch(t *testing.T) {
	f := newStepUpFixture(t)
	if _, err := f.pool.Exec(f.ctx, `UPDATE client_devices SET session_auth_at = NOW() - INTERVAL '3 hours' WHERE id = $1`, f.deviceID); err != nil {
		t.Fatal(err)
	}
	var connID string
	if err := f.pool.QueryRow(f.ctx,
		`INSERT INTO identity_connections (tenant_id, protocol, provider, display_name, issuer, client_id)
		 VALUES ($1, 'oidc', 'oidc', 'Test IdP', 'https://idp.test', 'cid') RETURNING id::text`, f.wsID).Scan(&connID); err != nil {
		t.Fatalf("insert connection: %v", err)
	}
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO external_identities (tenant_id, user_id, connection_id, issuer, subject)
		 VALUES ($1, $2, $3, 'https://idp.test', 'sub-owner')`, f.wsID, f.userID, connID); err != nil {
		t.Fatalf("insert identity: %v", err)
	}
	ra := f.reauth()
	sess := &authSession{WorkspaceID: f.wsID, ConnectionID: connID, Reauth: &ra}
	run := func(subject string, amr []string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/clients/callback", nil)
		f.svc.finishReauthIdP(rec, req, sess, connID, &providers.AuthenticationContext{
			Subject: subject, Email: "history@example.test", AMR: amr,
		})
		return rec
	}

	// Someone else signed in at the IdP: refused, audited, nothing stamped.
	rec := run("sub-someone-else", nil)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "different user") {
		t.Fatalf("identity mismatch: %d %s", rec.Code, rec.Body.String())
	}
	if d := f.device(); time.Since(*d.authAt) < time.Hour {
		t.Fatal("a mismatched identity stamped the device")
	}
	if f.audits(ActionStepUpFailed) != 1 {
		t.Fatalf("step_up_failed audit rows = %d, want 1", f.audits(ActionStepUpFailed))
	}

	// The owner, no policy requires MFA: authenticated without a TOTP page.
	rec = run("sub-owner", []string{"pwd"})
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), `name="code"`) {
		t.Fatalf("owner without MFA requirement: %d %s", rec.Code, rec.Body.String())
	}
	d := f.device()
	if time.Since(*d.authAt) > time.Minute || d.mfaAt != nil {
		t.Fatalf("after re-authentication: %+v", d)
	}

	// M-3: the IdP proved MFA, so the device's MFA is stamped with no TOTP.
	rec = run("sub-owner", []string{"pwd", "mfa"})
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), `name="code"`) {
		t.Fatalf("owner with IdP MFA: %d %s", rec.Code, rec.Body.String())
	}
	if d := f.device(); d.mfaAt == nil || time.Since(*d.mfaAt) > time.Minute {
		t.Fatalf("IdP MFA not recorded: %+v", d)
	}

	// M-9: the risk score demanded this authentication and the user has an
	// authenticator → the TOTP page is mandatory (no Skip), and nothing is
	// stamped until the code is verified.
	enr, err := f.mfaSvc.StartEnrollment(f.ctx, f.userID, f.wsID, "history@example.test")
	if err != nil {
		t.Fatal(err)
	}
	secret, _ := totp.DecodeSecret(enr.Secret)
	if _, _, err := f.mfaSvc.Verify(f.ctx, f.userID, totp.Code(secret, time.Now())); err != nil {
		t.Fatalf("confirm authenticator: %v", err)
	}
	if _, err := f.pool.Exec(f.ctx,
		`UPDATE client_devices SET risk_score = 50, reauth_required_at = NOW(), session_auth_at = NOW() - INTERVAL '1 hour' WHERE id = $1`,
		f.deviceID); err != nil {
		t.Fatal(err)
	}
	rec = run("sub-owner", []string{"pwd"})
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, `name="code"`) || strings.Contains(body, "Skip for now") {
		t.Fatalf("risk-demanded step-up must show a mandatory TOTP page: %d %s", rec.Code, body)
	}
	if strings.Contains(body, enr.Secret) {
		t.Fatal("the page showed an enrolled user's secret")
	}
	if d := f.device(); d.reauthAt == nil || time.Since(*d.authAt) < 30*time.Minute {
		t.Fatalf("the device was stamped before the code was verified: %+v", d)
	}
}

func TestStartReauth_Preconditions(t *testing.T) {
	f := newStepUpFixture(t)
	// Missing arguments.
	if _, err := f.svc.StartReauth(f.ctx, &clientv1.StartReauthRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty request: %v", err)
	}
	// A device of nobody.
	if _, err := f.svc.StartReauth(f.ctx, &clientv1.StartReauthRequest{
		AccessToken: "t", DeviceId: "00000000-0000-0000-0000-000000000000"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("unknown device: %v", err)
	}
	// A revoked device.
	if _, err := f.pool.Exec(f.ctx, `UPDATE client_devices SET revoked_at = NOW() WHERE id = $1`, f.deviceID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.StartReauth(f.ctx, &clientv1.StartReauthRequest{AccessToken: "t", DeviceId: f.deviceID}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("revoked device: %v", err)
	}
	// Not enabled.
	f.svc.mfaSvc = nil
	if _, err := f.svc.StartReauth(f.ctx, &clientv1.StartReauthRequest{AccessToken: "t", DeviceId: f.deviceID}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("disabled: %v", err)
	}
}
