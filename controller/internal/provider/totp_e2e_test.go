package provider_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourorg/ztna/controller/internal/appmeta"
	"github.com/yourorg/ztna/controller/internal/middleware"
	"github.com/yourorg/ztna/controller/internal/provider"
)

// Sprint 22 Phase T end to end over the real store (throwaway DB), middleware
// and handlers, wired like cmd/server/main.go. Decisions: OQ-1 (super-admin
// resets another operator's TOTP, with a reason), OQ-2 (no mandatory TOTP for
// relay-ops), OQ-3 (no grace period; old sessions rejected at once).

// fakeSealer stands in for pki.EncryptSecret/DecryptSecret: reversible, and it
// refuses to open a value under a different context, like the real AES-GCM
// derivation. It records every context it was asked to use.
type fakeSealer struct {
	mu       sync.Mutex
	contexts []string
}

func (f *fakeSealer) EncryptSecret(pt []byte, context string) (string, string, error) {
	f.mu.Lock()
	f.contexts = append(f.contexts, context)
	f.mu.Unlock()
	return base64.StdEncoding.EncodeToString([]byte(context + "|" + string(pt))), "nonce", nil
}

func (f *fakeSealer) DecryptSecret(ct, nonce, context string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(ct)
	if err != nil || nonce != "nonce" {
		return nil, errors.New("bad ciphertext")
	}
	prefix := context + "|"
	if !strings.HasPrefix(string(raw), prefix) {
		return nil, errors.New("context mismatch")
	}
	return raw[len(prefix):], nil
}

// countingLimiter blocks once `max` failures were recorded for an email.
type countingLimiter struct {
	mu       sync.Mutex
	max      int
	failures map[string]int
}

func newCountingLimiter(max int) *countingLimiter {
	return &countingLimiter{max: max, failures: map[string]int{}}
}

func (l *countingLimiter) Blocked(_ context.Context, email, _ string) (bool, time.Duration, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.failures[strings.ToLower(email)] >= l.max, 15 * time.Minute, nil
}

func (l *countingLimiter) RecordFailure(_ context.Context, email, _ string) ([]provider.LimitTrip, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := strings.ToLower(email)
	l.failures[e]++
	if l.failures[e] == l.max {
		return []provider.LimitTrip{{Scope: provider.LimitScopeEmail, Value: e, Failures: int64(l.max), RetryAfter: 15 * time.Minute}}, nil
	}
	return nil, nil
}

func (l *countingLimiter) Reset(_ context.Context, email string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.failures, strings.ToLower(email))
	return nil
}

type totpEnv struct {
	srv     *httptest.Server
	pool    *pgxpool.Pool
	store   *provider.Store
	ids     *provider.IdentityService
	sealer  *fakeSealer
	limiter *countingLimiter
	clock   time.Time
	admin   string // full (TOTP-backed) token of the bootstrap super-admin
	secrets map[string][]byte
}

func newTOTPEnv(t *testing.T) *totpEnv {
	t.Helper()
	store, pool := provider.NewTestStore(t)
	ctx := context.Background()
	if res, _, err := provider.Bootstrap(ctx, store, e2eEmail, e2eTempPass); err != nil || res != provider.BootstrapCreated {
		t.Fatalf("bootstrap: res=%v err=%v", res, err)
	}
	ids, err := provider.NewIdentityService(e2eKey)
	if err != nil {
		t.Fatal(err)
	}
	env := &totpEnv{pool: pool, store: store, ids: ids, sealer: &fakeSealer{}, limiter: newCountingLimiter(5),
		clock: time.Unix(1_800_000_000, 0), secrets: map[string][]byte{}}

	authz := provider.NewAuthz()
	auth := provider.NewAuthHandlers(ids, provider.NewLocalPasswordAuthenticator(store), store, allowAllLimiter{})
	totp := provider.NewTOTPHandlers(ids, store, env.sealer, env.limiter)
	provider.SetTOTPClock(totp, func() time.Time { return env.clock })
	handlers := provider.NewHandlers(store, authz)
	ops := provider.NewOperatorHandlers(store, authz)
	require := middleware.RequireProvider(ids, store)

	mux := http.NewServeMux()
	mux.Handle("POST /provider/auth/login", http.HandlerFunc(auth.Login))
	mux.Handle("POST /provider/auth/password",
		middleware.RequireProvider(ids, store, middleware.AllowPasswordChangeToken())(http.HandlerFunc(auth.ChangePassword)))
	mux.Handle("POST /provider/auth/totp/verify",
		middleware.RequireProvider(ids, store, middleware.AllowMFAToken(provider.MFAStageVerify))(http.HandlerFunc(totp.Verify)))
	mux.Handle("POST /provider/auth/totp/enroll",
		middleware.RequireProvider(ids, store, middleware.AllowMFAToken(provider.MFAStageEnroll))(http.HandlerFunc(totp.Enroll)))
	mux.Handle("POST /provider/auth/totp/enroll/confirm",
		middleware.RequireProvider(ids, store, middleware.AllowMFAToken(provider.MFAStageEnroll))(http.HandlerFunc(totp.ConfirmEnroll)))
	mux.Handle("GET /provider/me", require(http.HandlerFunc(handlers.Me)))
	mux.Handle("GET /provider/users", require(http.HandlerFunc(handlers.ListUsers)))
	mux.Handle("POST /provider/users", require(http.HandlerFunc(ops.Create)))
	mux.Handle("PATCH /provider/users/{id}", require(http.HandlerFunc(ops.ChangeRole)))
	mux.Handle("POST /provider/users/{id}/disable", require(http.HandlerFunc(ops.Disable)))
	mux.Handle("POST /provider/users/{id}/reset-totp", require(http.HandlerFunc(ops.ResetTOTP)))
	env.srv = httptest.NewServer(mux)
	t.Cleanup(env.srv.Close)

	env.admin = env.activateSuperAdmin(t, e2eEmail, e2eTempPass, e2eFinalPass)
	return env
}

type mfaSession struct {
	Token                  string `json:"token"`
	PasswordChangeRequired bool   `json:"password_change_required"`
	MFA                    string `json:"mfa"`
}

func decode[T any](t *testing.T, body string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	return v
}

// tick moves the TOTP clock one step, so the next code is fresh.
func (e *totpEnv) tick() { e.clock = e.clock.Add(30 * time.Second) }

func (e *totpEnv) code(email string) string {
	return provider.TOTPCode(e.secrets[email], e.clock)
}

func (e *totpEnv) login(t *testing.T, email, password string) (int, mfaSession) {
	t.Helper()
	code, body := call(t, e.srv, http.MethodPost, "/provider/auth/login", "", map[string]string{"email": email, "password": password})
	var s mfaSession
	_ = json.Unmarshal([]byte(body), &s)
	return code, s
}

// enrol completes an mfa:"enroll" token: start, remember the secret, confirm.
func (e *totpEnv) enrol(t *testing.T, email, enrollToken string) string {
	t.Helper()
	code, body := call(t, e.srv, http.MethodPost, "/provider/auth/totp/enroll", enrollToken, nil)
	if code != http.StatusOK {
		t.Fatalf("enroll %s: %d %s", email, code, body)
	}
	started := decode[struct {
		Secret     string `json:"secret"`
		OTPAuthURI string `json:"otpauth_uri"`
	}](t, body)
	secret, err := provider.DecodeTOTPSecret(started.Secret)
	if err != nil || !strings.HasPrefix(started.OTPAuthURI, "otpauth://totp/") {
		t.Fatalf("enroll response %s: %v", body, err)
	}
	e.secrets[email] = secret
	e.tick()
	code, body = call(t, e.srv, http.MethodPost, "/provider/auth/totp/enroll/confirm", enrollToken, map[string]string{"code": e.code(email)})
	if code != http.StatusOK {
		t.Fatalf("confirm %s: %d %s", email, code, body)
	}
	full := decode[mfaSession](t, body)
	if full.MFA != "" || full.Token == "" {
		t.Fatalf("confirm %s returned %+v", email, full)
	}
	return full.Token
}

// activateSuperAdmin: temporary password → forced change → forced enrolment.
func (e *totpEnv) activateSuperAdmin(t *testing.T, email, temp, final string) string {
	t.Helper()
	code, s := e.login(t, email, temp)
	if code != http.StatusOK || !s.PasswordChangeRequired {
		t.Fatalf("login %s with temporary password: %d %+v", email, code, s)
	}
	code, body := call(t, e.srv, http.MethodPost, "/provider/auth/password", s.Token,
		map[string]string{"current_password": temp, "new_password": final})
	if code != http.StatusOK {
		t.Fatalf("forced change for %s: %d %s", email, code, body)
	}
	after := decode[mfaSession](t, body)
	if after.MFA != provider.MFAStageEnroll {
		t.Fatalf("super-admin after password change: want mfa=enroll, got %+v", after)
	}
	return e.enrol(t, email, after.Token)
}

// signIn performs password + TOTP for an enrolled operator.
func (e *totpEnv) signIn(t *testing.T, email, password string) string {
	t.Helper()
	code, s := e.login(t, email, password)
	if code != http.StatusOK || s.MFA != provider.MFAStageVerify {
		t.Fatalf("login %s: %d %+v", email, code, s)
	}
	e.tick()
	code, body := call(t, e.srv, http.MethodPost, "/provider/auth/totp/verify", s.Token, map[string]string{"code": e.code(email)})
	if code != http.StatusOK {
		t.Fatalf("verify %s: %d %s", email, code, body)
	}
	return decode[mfaSession](t, body).Token
}

func (e *totpEnv) createOperator(t *testing.T, email, role string) (id, temp string) {
	t.Helper()
	code, body := call(t, e.srv, http.MethodPost, "/provider/users", e.admin, map[string]string{"email": email, "role": role})
	if code != http.StatusCreated {
		t.Fatalf("create %s: %d %s", email, code, body)
	}
	c := decode[createdOperator](t, body)
	return c.User.ID, c.TemporaryPassword
}

func (e *totpEnv) auditCount(t *testing.T, action string) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM provider_audit_logs WHERE action = $1`, action).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (e *totpEnv) allAuditText(t *testing.T) string {
	t.Helper()
	var s string
	if err := e.pool.QueryRow(context.Background(),
		`SELECT COALESCE(string_agg(action || ' ' || target_id || ' ' || COALESCE(details::text, ''), E'\n'), '') FROM provider_audit_logs`).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func (e *totpEnv) generation(t *testing.T, email string) int64 {
	t.Helper()
	var g int64
	if err := e.pool.QueryRow(context.Background(),
		`SELECT session_generation FROM provider_users WHERE email = $1`, email).Scan(&g); err != nil {
		t.Fatal(err)
	}
	return g
}

// --- enrolment ---

func TestTOTP_BootstrapSuperAdminMustEnrol(t *testing.T) {
	e := newTOTPEnv(t) // activateSuperAdmin asserts pwc → mfa=enroll → enrol → full

	if code, body := call(t, e.srv, http.MethodGet, "/provider/me", e.admin, nil); code != http.StatusOK || !strings.Contains(body, e2eEmail) {
		t.Fatalf("/provider/me with the TOTP-backed token: %d %s", code, body)
	}
	if n := e.auditCount(t, provider.AuditTOTPEnroll); n != 1 {
		t.Fatalf("totp_enroll audit rows: %d", n)
	}
	if len(e.sealer.contexts) == 0 {
		t.Fatal("the TOTP secret was never sealed")
	}
	var id string
	_ = e.pool.QueryRow(context.Background(), `SELECT id::text FROM provider_users WHERE email=$1`, e2eEmail).Scan(&id)
	for _, c := range e.sealer.contexts {
		if c != "provider-totp:"+id {
			t.Fatalf("seal context %q, want provider-totp:<user id>", c)
		}
	}
	// The secret never reaches the audit log; the column holds ciphertext only.
	if b32 := provider.EncodeTOTPSecret(e.secrets[e2eEmail]); strings.Contains(e.allAuditText(t), b32) {
		t.Fatal("TOTP secret found in the audit log")
	}
	var stored string
	_ = e.pool.QueryRow(context.Background(), `SELECT totp_secret_ciphertext FROM provider_users WHERE email=$1`, e2eEmail).Scan(&stored)
	if stored == "" || strings.Contains(stored, provider.EncodeTOTPSecret(e.secrets[e2eEmail])) || strings.Contains(stored, string(e.secrets[e2eEmail])) {
		t.Fatalf("stored TOTP secret is empty or not sealed: %q", stored)
	}
	// Operators see whether TOTP is enabled.
	_, body := call(t, e.srv, http.MethodGet, "/provider/users", e.admin, nil)
	if !strings.Contains(body, `"totp_enabled":true`) {
		t.Fatalf("operator list lacks totp_enabled: %s", body)
	}
}

func TestTOTP_EnrolTokenIsConfinedAndRevokedOnConfirm(t *testing.T) {
	e := newTOTPEnv(t)
	id, temp := e.createOperator(t, "second@inkyank.com", provider.RoleSuperAdmin)
	_ = id
	_, s := e.login(t, "second@inkyank.com", temp)
	_, body := call(t, e.srv, http.MethodPost, "/provider/auth/password", s.Token,
		map[string]string{"current_password": temp, "new_password": "second-final-pass-1"})
	enrollTok := decode[mfaSession](t, body).Token

	if code, body := call(t, e.srv, http.MethodGet, "/provider/me", enrollTok, nil); code != http.StatusForbidden || !strings.Contains(body, "mfa_required") {
		t.Fatalf("enroll token on /provider/me: %d %s", code, body)
	}
	// A wrong confirmation code is refused and counted.
	call(t, e.srv, http.MethodPost, "/provider/auth/totp/enroll", enrollTok, nil)
	if code, body := call(t, e.srv, http.MethodPost, "/provider/auth/totp/enroll/confirm", enrollTok, map[string]string{"code": "000000"}); code != http.StatusUnauthorized || errCode(body) != "invalid_code" {
		t.Fatalf("wrong confirm code: %d %s", code, body)
	}
	full := e.enrol(t, "second@inkyank.com", enrollTok)
	if code, _ := call(t, e.srv, http.MethodGet, "/provider/me", full, nil); code != http.StatusOK {
		t.Fatalf("full token after enrolment: %d", code)
	}
	if code, _ := call(t, e.srv, http.MethodPost, "/provider/auth/totp/enroll", enrollTok, nil); code != http.StatusUnauthorized {
		t.Fatalf("enroll token after confirm must be revoked (generation bump): %d", code)
	}
	// An enrolled operator can't restart enrolment, even with a fresh enroll-stage token.
	u, err := e.store.GetByEmail(context.Background(), "second@inkyank.com")
	if err != nil {
		t.Fatal(err)
	}
	forged, _, err := e.ids.IssueMFAToken(u, provider.MFAStageEnroll)
	if err != nil {
		t.Fatal(err)
	}
	if code, body := call(t, e.srv, http.MethodPost, "/provider/auth/totp/enroll", forged, nil); code != http.StatusConflict || errCode(body) != "totp_already_enabled" {
		t.Fatalf("re-enrol an enrolled operator: %d %s", code, body)
	}
}

// --- verification ---

func TestTOTP_SignInVerifyInvalidAndReplay(t *testing.T) {
	e := newTOTPEnv(t)
	code, s := e.login(t, e2eEmail, e2eFinalPass)
	if code != http.StatusOK || s.MFA != provider.MFAStageVerify || s.Token == "" {
		t.Fatalf("enrolled super-admin login: %d %+v", code, s)
	}
	if code, _ := call(t, e.srv, http.MethodGet, "/provider/me", s.Token, nil); code != http.StatusForbidden {
		t.Fatalf("verify token on /provider/me: %d", code)
	}
	if code, body := call(t, e.srv, http.MethodPost, "/provider/auth/totp/verify", s.Token, map[string]string{"code": "123456"}); code != http.StatusUnauthorized || errCode(body) != "invalid_code" {
		t.Fatalf("wrong code: %d %s", code, body)
	}
	e.tick()
	good := e.code(e2eEmail)
	code, body := call(t, e.srv, http.MethodPost, "/provider/auth/totp/verify", s.Token, map[string]string{"code": good})
	if code != http.StatusOK {
		t.Fatalf("right code: %d %s", code, body)
	}
	full := decode[mfaSession](t, body).Token
	claims, err := e.ids.Verify(full)
	if err != nil || len(claims.AMR) != 2 || claims.AMR[1] != provider.AMROTP {
		t.Fatalf("verified token amr: %+v %v", claims, err)
	}
	if code, _ := call(t, e.srv, http.MethodGet, "/provider/me", full, nil); code != http.StatusOK {
		t.Fatalf("/provider/me after verify: %d", code)
	}
	if n := e.auditCount(t, provider.AuditMFAVerify); n != 1 {
		t.Fatalf("mfa_verify audit rows: %d", n)
	}

	// Replay: the same code, from a new sign-in, in the same step.
	_, again := e.login(t, e2eEmail, e2eFinalPass)
	if code, body := call(t, e.srv, http.MethodPost, "/provider/auth/totp/verify", again.Token, map[string]string{"code": good}); code != http.StatusUnauthorized || errCode(body) != "invalid_code" {
		t.Fatalf("replayed code: %d %s", code, body)
	}
	e.tick()
	if code, _ := call(t, e.srv, http.MethodPost, "/provider/auth/totp/verify", again.Token, map[string]string{"code": e.code(e2eEmail)}); code != http.StatusOK {
		t.Fatalf("fresh code after the replay: %d", code)
	}
}

func TestTOTP_RateLimit(t *testing.T) {
	e := newTOTPEnv(t)
	_, s := e.login(t, e2eEmail, e2eFinalPass)
	for i := 0; i < 5; i++ {
		if code, body := call(t, e.srv, http.MethodPost, "/provider/auth/totp/verify", s.Token, map[string]string{"code": "000000"}); code != http.StatusUnauthorized {
			t.Fatalf("bad code %d: %d %s", i+1, code, body)
		}
	}
	e.tick()
	if code, body := call(t, e.srv, http.MethodPost, "/provider/auth/totp/verify", s.Token, map[string]string{"code": e.code(e2eEmail)}); code != http.StatusTooManyRequests || errCode(body) != "too_many_attempts" {
		t.Fatalf("after 5 bad codes even a good one is refused: %d %s", code, body)
	}
	if n := e.auditCount(t, provider.AuditTOTPRateLimit); n != 1 {
		t.Fatalf("totp rate-limit audit rows: %d", n)
	}
}

// --- OQ-3: no bypass ---

func TestTOTP_LegacySuperAdminSessionRejected(t *testing.T) {
	e := newTOTPEnv(t)
	u, err := e.store.GetByEmail(context.Background(), e2eEmail)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	legacy, err := jwt.NewWithClaims(jwt.SigningMethodHS256, provider.ProviderClaims{
		Role: u.Role, Email: u.Email, Gen: u.SessionGeneration, AMR: []string{provider.AMRPassword},
		RegisteredClaims: jwt.RegisteredClaims{
			Subject: u.ID, Issuer: appmeta.ProviderIssuer, Audience: jwt.ClaimStrings{provider.ProviderAudience},
			IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(10 * time.Minute)),
		},
	}).SignedString([]byte(e2eKey))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/provider/me", "/provider/users"} {
		if code, body := call(t, e.srv, http.MethodGet, path, legacy, nil); code != http.StatusForbidden || !strings.Contains(body, "mfa_required") {
			t.Fatalf("pre-deploy super-admin token on %s: %d %s", path, code, body)
		}
	}
}

func TestTOTP_PasswordChangeCannotBypass(t *testing.T) {
	e := newTOTPEnv(t)
	code, body := call(t, e.srv, http.MethodPost, "/provider/auth/password", e.admin,
		map[string]string{"current_password": e2eFinalPass, "new_password": "brand-new-pass-12"})
	if code != http.StatusOK {
		t.Fatalf("voluntary change: %d %s", code, body)
	}
	s := decode[mfaSession](t, body)
	if s.MFA != provider.MFAStageVerify {
		t.Fatalf("a super-admin's password change must end in the TOTP step, got %+v", s)
	}
	if code, _ := call(t, e.srv, http.MethodGet, "/provider/me", s.Token, nil); code != http.StatusForbidden {
		t.Fatalf("token from a password change on /provider/me: %d", code)
	}
}

func TestTOTP_RelayOpsSignsInWithoutTOTP(t *testing.T) {
	e := newTOTPEnv(t)
	_, temp := e.createOperator(t, "relay@inkyank.com", provider.RoleRelayOps)
	_, s := e.login(t, "relay@inkyank.com", temp)
	code, body := call(t, e.srv, http.MethodPost, "/provider/auth/password", s.Token,
		map[string]string{"current_password": temp, "new_password": "relay-final-pass-1"})
	full := decode[mfaSession](t, body)
	if code != http.StatusOK || full.MFA != "" || full.Token == "" {
		t.Fatalf("relay-ops after password change: %d %+v", code, full)
	}
	if code, _ := call(t, e.srv, http.MethodGet, "/provider/me", full.Token, nil); code != http.StatusOK {
		t.Fatalf("relay-ops /provider/me: %d", code)
	}
	if code, s := e.login(t, "relay@inkyank.com", "relay-final-pass-1"); code != http.StatusOK || s.MFA != "" {
		t.Fatalf("relay-ops sign-in asked for TOTP: %d %+v", code, s)
	}
}

func TestTOTP_DemotedSuperAdminStillVerifies(t *testing.T) {
	e := newTOTPEnv(t)
	id, temp := e.createOperator(t, "second@inkyank.com", provider.RoleSuperAdmin)
	e.activateSuperAdmin(t, "second@inkyank.com", temp, "second-final-pass-1")
	if code, body := call(t, e.srv, http.MethodPatch, "/provider/users/"+id, e.admin, map[string]string{"role": provider.RoleRelayOps}); code != http.StatusOK {
		t.Fatalf("demote: %d %s", code, body)
	}
	if code, s := e.login(t, "second@inkyank.com", "second-final-pass-1"); code != http.StatusOK || s.MFA != provider.MFAStageVerify {
		t.Fatalf("demoted operator with TOTP enabled must verify: %d %+v", code, s)
	}
}

// --- OQ-1: TOTP reset ---

func TestResetTOTP_ByAnotherSuperAdmin(t *testing.T) {
	e := newTOTPEnv(t)
	id, temp := e.createOperator(t, "second@inkyank.com", provider.RoleSuperAdmin)
	targetTok := e.activateSuperAdmin(t, "second@inkyank.com", temp, "second-final-pass-1")
	genBefore := e.generation(t, "second@inkyank.com")

	code, body := call(t, e.srv, http.MethodPost, "/provider/users/"+id+"/reset-totp", e.admin, map[string]string{"reason": "lost phone"})
	if code != http.StatusNoContent {
		t.Fatalf("reset-totp: %d %s", code, body)
	}
	if code, _ := call(t, e.srv, http.MethodGet, "/provider/me", targetTok, nil); code != http.StatusUnauthorized {
		t.Fatalf("target's session after reset: %d (want 401)", code)
	}
	if e.generation(t, "second@inkyank.com") <= genBefore {
		t.Fatal("reset did not bump the target's generation")
	}
	var ct *string
	var enabled *time.Time
	var last *int64
	_ = e.pool.QueryRow(context.Background(),
		`SELECT totp_secret_ciphertext, totp_enabled_at, totp_last_step FROM provider_users WHERE id=$1`, id).Scan(&ct, &enabled, &last)
	if ct != nil || enabled != nil || last != nil {
		t.Fatalf("enrolment not cleared: ct=%v enabled=%v last=%v", ct, enabled, last)
	}
	if code, s := e.login(t, "second@inkyank.com", "second-final-pass-1"); code != http.StatusOK || s.MFA != provider.MFAStageEnroll {
		t.Fatalf("after reset the target must enrol afresh: %d %+v", code, s)
	}
	var actor, target, details string
	var at time.Time
	if err := e.pool.QueryRow(context.Background(),
		`SELECT provider_email, target_id, details::text, created_at FROM provider_audit_logs WHERE action=$1`, provider.AuditTOTPReset).Scan(&actor, &target, &details, &at); err != nil {
		t.Fatalf("totp_reset audit row: %v", err)
	}
	if actor != e2eEmail || target != id || !strings.Contains(details, "lost phone") || !strings.Contains(details, "second@inkyank.com") || at.IsZero() {
		t.Fatalf("audit row: actor=%s target=%s details=%s at=%v", actor, target, details, at)
	}
}

func TestResetTOTP_Guards(t *testing.T) {
	e := newTOTPEnv(t)
	var adminID string
	_ = e.pool.QueryRow(context.Background(), `SELECT id::text FROM provider_users WHERE email=$1`, e2eEmail).Scan(&adminID)
	secondID, temp := e.createOperator(t, "second@inkyank.com", provider.RoleSuperAdmin)
	e.activateSuperAdmin(t, "second@inkyank.com", temp, "second-final-pass-1")
	relayID, relayTemp := e.createOperator(t, "relay@inkyank.com", provider.RoleRelayOps)
	_, s := e.login(t, "relay@inkyank.com", relayTemp)
	_, body := call(t, e.srv, http.MethodPost, "/provider/auth/password", s.Token,
		map[string]string{"current_password": relayTemp, "new_password": "relay-final-pass-1"})
	relayTok := decode[mfaSession](t, body).Token
	before := e.auditCount(t, provider.AuditTOTPReset)

	for _, tc := range []struct {
		name, token, id string
		body            any
		status          int
		code            string
	}{
		{"relay-ops actor", relayTok, secondID, map[string]string{"reason": "x"}, http.StatusForbidden, "forbidden"},
		{"self", e.admin, adminID, map[string]string{"reason": "x"}, http.StatusConflict, "cannot_modify_self"},
		{"no reason", e.admin, secondID, map[string]string{}, http.StatusBadRequest, "reason_required"},
		{"blank reason", e.admin, secondID, map[string]string{"reason": "   "}, http.StatusBadRequest, "reason_required"},
		{"long reason", e.admin, secondID, map[string]string{"reason": strings.Repeat("r", 501)}, http.StatusBadRequest, "reason_too_long"},
		{"not enrolled", e.admin, relayID, map[string]string{"reason": "x"}, http.StatusConflict, "totp_not_enrolled"},
		{"unknown", e.admin, "00000000-0000-4000-8000-000000000000", map[string]string{"reason": "x"}, http.StatusNotFound, "not_found"},
	} {
		code, body := call(t, e.srv, http.MethodPost, "/provider/users/"+tc.id+"/reset-totp", tc.token, tc.body)
		if code != tc.status || errCode(body) != tc.code {
			t.Errorf("%s: %d %s, want %d %s", tc.name, code, body, tc.status, tc.code)
		}
	}
	// A disabled target is refused (enable, then reset), like password reset.
	if code, _ := call(t, e.srv, http.MethodPost, "/provider/users/"+secondID+"/disable", e.admin, nil); code != http.StatusNoContent {
		t.Fatalf("disable: %d", code)
	}
	if code, body := call(t, e.srv, http.MethodPost, "/provider/users/"+secondID+"/reset-totp", e.admin, map[string]string{"reason": "x"}); code != http.StatusConflict || errCode(body) != "account_disabled" {
		t.Fatalf("disabled target: %d %s", code, body)
	}
	if n := e.auditCount(t, provider.AuditTOTPReset); n != before {
		t.Fatalf("refused resets wrote %d audit rows", n-before)
	}
}

// --- recover-admin (break-glass) ---

func TestRecoverAdmin_ResetsPasswordAndTOTP(t *testing.T) {
	e := newTOTPEnv(t)
	ctx := context.Background()
	temp, err := provider.RecoverAdmin(ctx, e.store, "  "+strings.ToUpper(e2eEmail)+" ")
	if err != nil || len(temp) < 12 {
		t.Fatalf("recover-admin: %q %v", temp, err)
	}
	if code, _ := call(t, e.srv, http.MethodGet, "/provider/me", e.admin, nil); code != http.StatusUnauthorized {
		t.Fatalf("old session after recovery: %d (want 401)", code)
	}
	code, s := e.login(t, e2eEmail, temp)
	if code != http.StatusOK || !s.PasswordChangeRequired {
		t.Fatalf("login with the recovery password: %d %+v", code, s)
	}
	_, body := call(t, e.srv, http.MethodPost, "/provider/auth/password", s.Token,
		map[string]string{"current_password": temp, "new_password": "recovered-pass-123"})
	if after := decode[mfaSession](t, body); after.MFA != provider.MFAStageEnroll {
		t.Fatalf("after recovery the super-admin must enrol afresh: %+v", after)
	}
	var actor, details string
	var uid *string
	if err := e.pool.QueryRow(ctx,
		`SELECT provider_email, provider_user_id::text, details::text FROM provider_audit_logs WHERE action=$1`, provider.AuditRecoverAdmin).Scan(&actor, &uid, &details); err != nil {
		t.Fatalf("recover_admin audit row: %v", err)
	}
	if actor != provider.SystemCLIActor || uid != nil || strings.Contains(details, temp) {
		t.Fatalf("audit row: actor=%s uid=%v details=%s", actor, uid, details)
	}
}

func TestRecoverAdmin_Boundaries(t *testing.T) {
	e := newTOTPEnv(t)
	ctx := context.Background()
	e.createOperator(t, "relay@inkyank.com", provider.RoleRelayOps)
	if _, err := provider.RecoverAdmin(ctx, e.store, "relay@inkyank.com"); !errors.Is(err, provider.ErrNotSuperAdmin) {
		t.Fatalf("relay-ops: %v, want ErrNotSuperAdmin", err)
	}
	if _, err := provider.RecoverAdmin(ctx, e.store, "nobody@inkyank.com"); !errors.Is(err, provider.ErrProviderUserNotFound) {
		t.Fatalf("unknown: %v, want ErrProviderUserNotFound", err)
	}
	id, temp := e.createOperator(t, "second@inkyank.com", provider.RoleSuperAdmin)
	e.activateSuperAdmin(t, "second@inkyank.com", temp, "second-final-pass-1")
	call(t, e.srv, http.MethodPost, "/provider/users/"+id+"/disable", e.admin, nil)
	if _, err := provider.RecoverAdmin(ctx, e.store, "second@inkyank.com"); !errors.Is(err, provider.ErrAccountDisabled) {
		t.Fatalf("disabled super-admin: %v, want ErrAccountDisabled", err)
	}
	if n := e.auditCount(t, provider.AuditRecoverAdmin); n != 0 {
		t.Fatalf("refused recoveries wrote %d audit rows", n)
	}
}
