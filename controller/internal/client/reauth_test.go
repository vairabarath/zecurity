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
	"github.com/yourorg/ztna/controller/internal/auth"
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

func (l *memLimiter) Attempt(_ context.Context, u string) (int64, time.Duration, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.n[u]++
	return int64(l.n[u]), time.Minute, nil
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
	lastSID  string // step-up session of the last reauth()
}

const (
	testStepUpVerifier = "test-step-up-verifier"
	testLoopback       = "http://127.0.0.1:5555/callback"
)

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
	sid, _ := newSessionID()
	f.lastSID = sid
	return reauthInfo{UserID: f.userID, DeviceID: f.deviceID, SID: "test-sid", Email: "history@example.test", StartedAt: time.Now(),
		SessionID: sid, CliCodeChallenge: sha256b64url([]byte(testStepUpVerifier)), LocalRedirectURI: testLoopback}
}

// loopbackCode checks that the browser was sent to the client's loopback
// address with a one-time code, and returns the code.
func (f *stepUpFixture) loopbackCode(rec *httptest.ResponseRecorder) string {
	f.t.Helper()
	if rec.Code != http.StatusSeeOther {
		f.t.Fatalf("want a 303 to the loopback, got %d %s", rec.Code, rec.Body.String())
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil || !strings.HasPrefix(loc.String(), testLoopback+"?code=") || loc.Query().Get("code") == "" {
		f.t.Fatalf("redirect = %q", rec.Header().Get("Location"))
	}
	return loc.Query().Get("code")
}

// redeem completes the step-up the way the client does.
func (f *stepUpFixture) redeem(rec *httptest.ResponseRecorder) (*clientv1.CompleteReauthResponse, error) {
	f.t.Helper()
	code := f.loopbackCode(rec)
	f.svc.authSvc = &postureTestAuth{claims: &auth.AccessTokenClaims{
		UserID: f.userID, TenantID: f.wsID, Email: "history@example.test", SessionID: "test-sid"}}
	return f.svc.CompleteReauth(f.ctx, &clientv1.CompleteReauthRequest{
		AccessToken: "tok", DeviceId: f.deviceID, SessionId: f.lastSID, CtrlCode: code, CodeVerifier: testStepUpVerifier})
}

func (f *stepUpFixture) mustRedeem(rec *httptest.ResponseRecorder) *clientv1.CompleteReauthResponse {
	f.t.Helper()
	res, err := f.redeem(rec)
	if err != nil {
		f.t.Fatalf("CompleteReauth: %v", err)
	}
	return res
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
	stepUpHeaders(rec, testLoopback)
	renderStepUp(rec, http.StatusOK, mfaPage(id, tk, ""))
	body := rec.Body.String()
	if !strings.Contains(body, tk.Enrollment.Secret) || !strings.Contains(body, "data:image/png;base64,") {
		t.Fatal("enrolment page lacks the key or the QR code")
	}
	if rec.Header().Get("Cache-Control") != "no-store" || !strings.Contains(rec.Header().Get("Content-Security-Policy"), "default-src 'none'") {
		t.Fatalf("enrolment page headers: %v", rec.Header())
	}
	if !strings.Contains(rec.Header().Get("Content-Security-Policy"), "form-action 'self' http://127.0.0.1:5555;") {
		t.Fatalf("form-action must allow only self and the client's loopback: %s", rec.Header().Get("Content-Security-Policy"))
	}

	// Wrong code: page again with an error, nothing stamped.
	rec = f.post(url.Values{"ticket": {id}, "code": {"000000"}, "action": {"verify"}})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "not valid") {
		t.Fatalf("wrong code: %d %s", rec.Code, rec.Body.String())
	}
	if d := f.device(); d.mfaAt != nil || d.reauthAt == nil {
		t.Fatalf("a wrong code changed the device: %+v", d)
	}

	// Right code: the browser goes to the client's loopback, and nothing is
	// stamped until the client redeems the code.
	rec = f.post(url.Values{"ticket": {id}, "code": {totp.Code(secret, time.Now())}, "action": {"verify"}})
	f.loopbackCode(rec)
	if d := f.device(); d.mfaAt != nil || d.reauthAt == nil {
		t.Fatalf("stamped before the client redeemed the code: %+v", d)
	}
	if res := f.mustRedeem(rec); !res.GetMfa() || res.GetAuthRequiredCount() != 0 {
		t.Fatalf("CompleteReauth = %+v", res)
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
	if res := f.mustRedeem(rec); res.GetMfa() {
		t.Fatalf("skip reported MFA: %+v", res)
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
	authTime := time.Now().Unix()
	run := func(subject string, amr []string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/clients/callback", nil)
		f.svc.finishReauthIdP(rec, req, sess, connID, &providers.AuthenticationContext{
			Subject: subject, Email: "history@example.test", AMR: amr, AuthTime: authTime,
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
	f.mustRedeem(rec)
	d := f.device()
	if time.Since(*d.authAt) > time.Minute || d.mfaAt != nil {
		t.Fatalf("after re-authentication: %+v", d)
	}

	// An amr from an IdP sign-in older than this step-up (a silent round trip
	// on a live IdP session) is not a second factor: MFA is not stamped.
	authTime = time.Now().Add(-2 * time.Hour).Unix()
	rec = run("sub-owner", []string{"pwd", "mfa"})
	if res := f.mustRedeem(rec); res.GetMfa() {
		t.Fatalf("stale IdP MFA reported as MFA: %+v", res)
	}
	if d := f.device(); d.mfaAt != nil {
		t.Fatalf("a stale IdP amr stamped MFA: %+v", d)
	}
	authTime = time.Now().Unix()

	// M-3: the IdP proved MFA during this step-up, so the device's MFA is
	// stamped with no TOTP.
	rec = run("sub-owner", []string{"pwd", "mfa"})
	if res := f.mustRedeem(rec); !res.GetMfa() {
		t.Fatalf("IdP MFA not reported: %+v", res)
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
	// The client must bind the step-up to a loopback address.
	for _, lr := range []string{"", "https://evil.example/cb", "http://10.0.0.5:5555/cb", "http://127.0.0.1/cb"} {
		if _, err := f.svc.StartReauth(f.ctx, &clientv1.StartReauthRequest{
			AccessToken: "t", DeviceId: f.deviceID, CodeChallenge: "c", LocalRedirectUri: lr}); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("local_redirect_uri %q: %v", lr, err)
		}
	}
	// A device of nobody.
	if _, err := f.svc.StartReauth(f.ctx, &clientv1.StartReauthRequest{
		AccessToken: "t", DeviceId: "00000000-0000-0000-0000-000000000000",
		CodeChallenge: "c", LocalRedirectUri: testLoopback}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("unknown device: %v", err)
	}
	// A revoked device.
	if _, err := f.pool.Exec(f.ctx, `UPDATE client_devices SET revoked_at = NOW() WHERE id = $1`, f.deviceID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.StartReauth(f.ctx, &clientv1.StartReauthRequest{AccessToken: "t", DeviceId: f.deviceID, CodeChallenge: "c", LocalRedirectUri: testLoopback}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("revoked device: %v", err)
	}
	// Not enabled.
	f.svc.mfaSvc = nil
	if _, err := f.svc.StartReauth(f.ctx, &clientv1.StartReauthRequest{AccessToken: "t", DeviceId: f.deviceID, CodeChallenge: "c", LocalRedirectUri: testLoopback}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("disabled: %v", err)
	}
}

// A step-up that answers a risk demand without a second factor drops the
// older MFA mark; without a risk demand the mark is kept.
func TestCompleteStepUp_RiskDemandDropsOldMFAMark(t *testing.T) {
	f := newStepUpFixture(t)
	ra := f.reauth()
	set := func(reauth string) {
		t.Helper()
		if _, err := f.pool.Exec(f.ctx,
			`UPDATE client_devices SET session_mfa_at = NOW() - INTERVAL '10 minutes',
			        reauth_required_at = `+reauth+` WHERE id = $1`, f.deviceID); err != nil {
			t.Fatal(err)
		}
	}
	set("NULL")
	if !f.svc.completeStepUp(f.ctx, f.wsID, &ra, false, "none") {
		t.Fatal("step-up refused")
	}
	if d := f.device(); d.mfaAt == nil {
		t.Fatal("a plain re-authentication dropped the MFA mark")
	}
	set("NOW() - INTERVAL '1 minute'")
	if !f.svc.completeStepUp(f.ctx, f.wsID, &ra, false, "none") {
		t.Fatal("step-up refused")
	}
	if d := f.device(); d.mfaAt != nil || d.reauthAt != nil {
		t.Fatalf("after a risk-demanded step-up without MFA: %+v, want no MFA mark and no demand", d)
	}
}

func TestIdPMFAFresh(t *testing.T) {
	start := time.Now()
	cases := []struct {
		name     string
		amr      bool
		authTime int64
		want     bool
	}{
		{"fresh", true, start.Unix(), true},
		{"within skew", true, start.Add(-30 * time.Second).Unix(), true},
		{"older than step-up", true, start.Add(-2 * time.Minute).Unix(), false},
		{"missing auth_time", true, 0, false},
		{"no amr mfa", false, start.Unix(), false},
	}
	for _, c := range cases {
		if got := idpMFAFresh(c.amr, c.authTime, start); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
	if idpMFAFresh(true, start.Unix(), time.Time{}) {
		t.Error("zero StartedAt must not count as fresh")
	}
}

// Finding (step-up phishing): the browser result is bound to the client that
// started the step-up. A wrong code, wrong verifier, another session or
// another device opens nothing, and every grant is single use.
func TestCompleteReauth_BoundToTheClient(t *testing.T) {
	f := newStepUpFixture(t)
	if _, err := f.pool.Exec(f.ctx, `UPDATE client_devices SET session_auth_at = NOW() - INTERVAL '3 hours' WHERE id = $1`, f.deviceID); err != nil {
		t.Fatal(err)
	}
	f.enrolTOTP()
	stale := func() {
		t.Helper()
		if d := f.device(); time.Since(*d.authAt) < time.Hour {
			t.Fatalf("the device was stamped: %+v", d)
		}
	}
	grant := func() (string, string) {
		ra := f.reauth()
		rec := httptest.NewRecorder()
		f.svc.grantStepUp(rec, httptest.NewRequest(http.MethodGet, "/api/clients/callback", nil), f.wsID, &ra, true, "totp")
		return f.lastSID, f.loopbackCode(rec)
	}
	call := func(sid, code, verifier, device string, claims *auth.AccessTokenClaims) error {
		f.svc.authSvc = &postureTestAuth{claims: claims}
		_, err := f.svc.CompleteReauth(f.ctx, &clientv1.CompleteReauthRequest{
			AccessToken: "tok", DeviceId: device, SessionId: sid, CtrlCode: code, CodeVerifier: verifier})
		return err
	}
	owner := &auth.AccessTokenClaims{UserID: f.userID, TenantID: f.wsID, Email: "history@example.test", SessionID: "test-sid"}

	// Nothing is stamped by the browser step itself.
	sid, code := grant()
	stale()

	// Wrong code: refused, audited, and the grant is gone.
	if err := call(sid, "wrong", testStepUpVerifier, f.deviceID, owner); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("wrong code: %v", err)
	}
	if err := call(sid, code, testStepUpVerifier, f.deviceID, owner); status.Code(err) != codes.NotFound {
		t.Fatalf("grant reused after a failed attempt: %v", err)
	}
	stale()

	// Wrong verifier (someone who only saw the code).
	sid, code = grant()
	if err := call(sid, code, "not-the-verifier", f.deviceID, owner); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("wrong verifier: %v", err)
	}
	stale()

	if n := f.audits(ActionStepUpFailed); n != 2 {
		t.Fatalf("step_up_failed audit rows = %d, want 2", n)
	}

	// Not the owner (another session, another device, another user or
	// workspace): NotFound, no audit row, and the grant is NOT consumed, so
	// a stranger with any valid token cannot burn the owner's step-up.
	sid, code = grant()
	strangers := map[string]struct {
		device string
		claims auth.AccessTokenClaims
	}{
		"other session":   {f.deviceID, auth.AccessTokenClaims{UserID: f.userID, TenantID: f.wsID, SessionID: "another-sid"}},
		"other device":    {"00000000-0000-0000-0000-000000000000", *owner},
		"other user":      {f.deviceID, auth.AccessTokenClaims{UserID: "00000000-0000-0000-0000-000000000001", TenantID: f.wsID, SessionID: "test-sid"}},
		"other workspace": {f.deviceID, auth.AccessTokenClaims{UserID: f.userID, TenantID: "00000000-0000-0000-0000-000000000002", SessionID: "test-sid"}},
	}
	for name, c := range strangers {
		cl := c.claims
		if err := call(sid, code, testStepUpVerifier, c.device, &cl); status.Code(err) != codes.NotFound {
			t.Fatalf("%s: %v", name, err)
		}
	}
	stale()
	if n := f.audits(ActionStepUpFailed); n != 2 {
		t.Fatalf("a stranger wrote audit rows: step_up_failed = %d, want 2", n)
	}

	// The real client, on the same grant the strangers tried: stamped, once.
	if err := call(sid, code, testStepUpVerifier, f.deviceID, owner); err != nil {
		t.Fatalf("redeem: %v", err)
	}
	if d := f.device(); time.Since(*d.authAt) > time.Minute || d.mfaAt == nil {
		t.Fatalf("not stamped: %+v", d)
	}
	if err := call(sid, code, testStepUpVerifier, f.deviceID, owner); status.Code(err) != codes.NotFound {
		t.Fatalf("second redeem: %v", err)
	}
	if n := f.audits(ActionStepUp); n != 1 {
		t.Fatalf("step_up audit rows = %d, want 1", n)
	}
}

// Review fix: an MFA reset withdraws the user's step-ups in flight, and only
// that user's.
func TestCancelUserStepUps(t *testing.T) {
	exp := time.Now().Add(time.Minute)
	putMFATicket("t-a", &mfaTicket{Reauth: reauthInfo{UserID: "user-a"}, ExpiresAt: exp})
	putMFATicket("t-b", &mfaTicket{Reauth: reauthInfo{UserID: "user-b"}, ExpiresAt: exp})
	putStepUpGrant("g-a", &stepUpGrant{Reauth: reauthInfo{UserID: "user-a"}, WithMFA: true, ExpiresAt: exp})
	putStepUpGrant("g-b", &stepUpGrant{Reauth: reauthInfo{UserID: "user-b"}, WithMFA: true, ExpiresAt: exp})
	t.Cleanup(func() { cancelUserStepUps("user-b") })

	cancelUserStepUps("user-a")

	if _, ok := getMFATicket("t-a"); ok {
		t.Fatal("user-a ticket survived the reset")
	}
	if _, ok := takeStepUpGrant("g-a", func(*stepUpGrant) bool { return true }); ok {
		t.Fatal("user-a grant survived the reset")
	}
	if _, ok := getMFATicket("t-b"); !ok {
		t.Fatal("user-b ticket was dropped")
	}
	if _, ok := takeStepUpGrant("g-b", func(*stepUpGrant) bool { return false }); ok {
		t.Fatal("owns=false must not take the grant")
	}
	stepUpGrantMu.Lock()
	_, ok := stepUpGrants["g-b"]
	stepUpGrantMu.Unlock()
	if !ok {
		t.Fatal("user-b grant was dropped")
	}
}

// enrolTOTP gives the fixture user a confirmed authenticator, as a TOTP
// step-up has.
func (f *stepUpFixture) enrolTOTP() {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO user_totp (user_id, workspace_id, secret_ciphertext, secret_nonce, enabled_at)
		 VALUES ($1, $2, 'ct', 'nonce', NOW())`, f.userID, f.wsID); err != nil {
		f.t.Fatalf("enrol totp: %v", err)
	}
}

// Review fix: a TOTP step-up redeemed after the user's authenticator was
// reset does not stamp MFA (the reset won the race).
func TestCompleteReauth_TOTPAfterResetStampsNoMFA(t *testing.T) {
	f := newStepUpFixture(t)
	ra := f.reauth()
	if !f.svc.completeStepUp(f.ctx, f.wsID, &ra, true, "totp") {
		t.Fatal("step-up not applied")
	}
	if d := f.device(); d.mfaAt != nil {
		t.Fatalf("MFA stamped without an authenticator: %+v", d)
	}
	f.enrolTOTP()
	if !f.svc.completeStepUp(f.ctx, f.wsID, &ra, true, "totp") {
		t.Fatal("step-up not applied")
	}
	if d := f.device(); d.mfaAt == nil {
		t.Fatalf("MFA not stamped with an authenticator: %+v", d)
	}
}
