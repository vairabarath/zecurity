package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// memStore is an in-memory stand-in for *Store with the same generation
// semantics (SetPassword and BumpSessionGeneration bump it).
type memStore struct {
	mu     sync.Mutex
	users  map[string]*ProviderUser // by id
	audits []AuditEntry
}

func newMemStore(users ...*ProviderUser) *memStore {
	m := &memStore{users: map[string]*ProviderUser{}}
	for _, u := range users {
		if u.SessionGeneration == 0 {
			u.SessionGeneration = 1
		}
		m.users[u.ID] = u
	}
	return m
}

func (m *memStore) GetByEmailForLogin(_ context.Context, email string) (*ProviderUser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, u := range m.users {
		if u.Email == normalizeEmail(email) {
			c := *u
			return &c, nil
		}
	}
	return nil, ErrProviderUserNotFound
}

func (m *memStore) GetByID(_ context.Context, id string) (*ProviderUser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.users[id]
	if !ok {
		return nil, ErrProviderUserNotFound
	}
	c := *u
	return &c, nil
}

func (m *memStore) SetPassword(_ context.Context, id, hash string, mustChange bool) (*ProviderUser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.users[id]
	if !ok {
		return nil, ErrProviderUserNotFound
	}
	u.PasswordHash, u.MustChangePassword = hash, mustChange
	u.SessionGeneration++
	c := *u
	return &c, nil
}

func (m *memStore) BumpSessionGeneration(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.users[id]
	if !ok {
		return ErrProviderUserNotFound
	}
	u.SessionGeneration++
	return nil
}

func (m *memStore) RecordLogin(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	m.users[id].LastLoginAt = &now
	return nil
}

func (m *memStore) InsertAudit(_ context.Context, e AuditEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.audits = append(m.audits, e)
	return nil
}

func (m *memStore) auditCount(action string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, a := range m.audits {
		if a.Action == action {
			n++
		}
	}
	return n
}

// fakeLimiter lets each test choose the limiter's behaviour.
type fakeLimiter struct {
	blocked    bool
	retry      time.Duration
	blockedErr error
	recordErr  error
	tripOn     int // trip the email scope on this failure number (0 = never)
	failures   int
	resets     int
}

func (f *fakeLimiter) Blocked(context.Context, string, string) (bool, time.Duration, error) {
	return f.blocked, f.retry, f.blockedErr
}

func (f *fakeLimiter) RecordFailure(_ context.Context, email, _ string) ([]LimitTrip, error) {
	if f.recordErr != nil {
		return nil, f.recordErr
	}
	f.failures++
	if f.tripOn != 0 && f.failures == f.tripOn {
		return []LimitTrip{{Scope: LimitScopeEmail, Value: normalizeEmail(email), Failures: int64(f.failures), RetryAfter: loginFailWindow}}, nil
	}
	return nil, nil
}

func (f *fakeLimiter) Reset(context.Context, string) error { f.resets++; return nil }

// countingAuthn wraps an Authenticator to prove when it is (not) consulted.
type countingAuthn struct {
	Authenticator
	calls int
}

func (c *countingAuthn) Authenticate(ctx context.Context, in Credentials) (*ProviderUser, error) {
	c.calls++
	return c.Authenticator.Authenticate(ctx, in)
}

type authFixture struct {
	h       *AuthHandlers
	store   *memStore
	limiter *fakeLimiter
	authn   *countingAuthn
	ids     *IdentityService
}

func newAuthFixture(t *testing.T, users ...*ProviderUser) *authFixture {
	t.Helper()
	store := newMemStore(users...)
	lim := &fakeLimiter{}
	ids := mustIdentity(t)
	authn := &countingAuthn{Authenticator: NewLocalPasswordAuthenticator(store)}
	return &authFixture{h: NewAuthHandlers(ids, authn, store, lim), store: store, limiter: lim, authn: authn, ids: ids}
}

func postJSON(t *testing.T, h http.HandlerFunc, body any, actor *Actor) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(b))
	req.RemoteAddr = "203.0.113.5:40000"
	if actor != nil {
		req = req.WithContext(WithActor(req.Context(), *actor))
	}
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func activeUser(t *testing.T, id, email, role, password string, mustChange bool) *ProviderUser {
	return &ProviderUser{ID: id, Email: email, Role: role, PasswordHash: mustHash(t, password), MustChangePassword: mustChange}
}

func TestLogin_Success_Audited(t *testing.T) {
	f := newAuthFixture(t, activeUser(t, "u1", "ops@inkyank.com", RoleSuperAdmin, "correct-password-1", false))
	rec := postJSON(t, f.h.Login, loginRequest{Email: "OPS@inkyank.com", Password: "correct-password-1"}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var resp sessionResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.PasswordChangeRequired || resp.ExpiresIn != int64(ProviderSessionTTL/time.Second) || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("response: %+v headers=%v", resp, rec.Header())
	}
	c, err := f.ids.Verify(resp.Token)
	if err != nil || c.Subject != "u1" || c.PWC || c.AMR[0] != AMRPassword {
		t.Fatalf("token: %+v err=%v", c, err)
	}
	if f.store.auditCount(AuditSessionLogin) != 1 || f.limiter.resets != 1 {
		t.Fatalf("audit=%d resets=%d", f.store.auditCount(AuditSessionLogin), f.limiter.resets)
	}
	if u, _ := f.store.GetByID(context.Background(), "u1"); u.LastLoginAt == nil {
		t.Fatal("last_login_at not recorded")
	}
}

func TestLogin_NoEnumeration(t *testing.T) {
	disabled := activeUser(t, "u2", "disabled@inkyank.com", RoleRelayOps, "correct-password-1", false)
	now := time.Now()
	disabled.DisabledAt = &now
	f := newAuthFixture(t,
		activeUser(t, "u1", "ok@inkyank.com", RoleRelayOps, "correct-password-1", false),
		disabled,
		&ProviderUser{ID: "u3", Email: "nohash@inkyank.com", Role: RoleRelayOps},
	)
	var bodies []string
	for _, req := range []loginRequest{
		{Email: "nobody@inkyank.com", Password: "correct-password-1"},
		{Email: "disabled@inkyank.com", Password: "correct-password-1"},
		{Email: "nohash@inkyank.com", Password: "correct-password-1"},
		{Email: "ok@inkyank.com", Password: "wrong-password-11"},
	} {
		rec := postJSON(t, f.h.Login, req, nil)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s: status %d", req.Email, rec.Code)
		}
		bodies = append(bodies, rec.Body.String())
	}
	for _, b := range bodies[1:] {
		if b != bodies[0] {
			t.Fatalf("responses differ (enumeration): %q vs %q", bodies[0], b)
		}
	}
	if !strings.Contains(bodies[0], "invalid_credentials") || f.limiter.failures != 4 {
		t.Fatalf("body=%s failures=%d", bodies[0], f.limiter.failures)
	}
}

// While locked out the password is not even checked: 429 + Retry-After.
func TestLogin_RateLimited(t *testing.T) {
	f := newAuthFixture(t, activeUser(t, "u1", "ops@inkyank.com", RoleSuperAdmin, "correct-password-1", false))
	f.limiter.blocked, f.limiter.retry = true, 90*time.Second+time.Millisecond

	rec := postJSON(t, f.h.Login, loginRequest{Email: "ops@inkyank.com", Password: "correct-password-1"}, nil)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "91" {
		t.Fatalf("status %d Retry-After=%q", rec.Code, rec.Header().Get("Retry-After"))
	}
	if f.authn.calls != 0 {
		t.Fatal("authenticator consulted while locked out")
	}
}

// The limiter is a security control: if it cannot answer or count, login
// fails closed with 503 rather than proceeding unprotected.
func TestLogin_ValkeyDown_FailsClosed(t *testing.T) {
	f := newAuthFixture(t, activeUser(t, "u1", "ops@inkyank.com", RoleSuperAdmin, "correct-password-1", false))
	f.limiter.blockedErr = errors.New("valkey down")
	if rec := postJSON(t, f.h.Login, loginRequest{Email: "ops@inkyank.com", Password: "correct-password-1"}, nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("Blocked error: status %d, want 503", rec.Code)
	}

	f.limiter.blockedErr, f.limiter.recordErr = nil, errors.New("valkey down")
	if rec := postJSON(t, f.h.Login, loginRequest{Email: "ops@inkyank.com", Password: "wrong-password-11"}, nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("RecordFailure error: status %d, want 503", rec.Code)
	}
}

// Exactly one provider_auth.rate_limit audit row when a lockout starts.
func TestLogin_RateLimitAuditedOnceOnTrip(t *testing.T) {
	f := newAuthFixture(t, activeUser(t, "u1", "ops@inkyank.com", RoleSuperAdmin, "correct-password-1", false))
	f.limiter.tripOn = loginFailMaxPerEmail
	for i := 0; i < loginFailMaxPerEmail+2; i++ {
		postJSON(t, f.h.Login, loginRequest{Email: "ops@inkyank.com", Password: "wrong-password-11"}, nil)
	}
	if n := f.store.auditCount(AuditRateLimit); n != 1 {
		t.Fatalf("rate-limit audit rows = %d, want 1", n)
	}
	for _, a := range f.store.audits {
		if a.Action == AuditRateLimit && (a.TargetID != "email:ops@inkyank.com" || a.IPAddress != "203.0.113.5" || a.ProviderUserID != nil) {
			t.Fatalf("rate-limit audit row: %+v", a)
		}
	}
}

func TestLogin_MustChange_ReturnsPasswordChangeToken(t *testing.T) {
	f := newAuthFixture(t, activeUser(t, "u1", "boot@inkyank.com", RoleSuperAdmin, "temporary-pass-1", true))
	rec := postJSON(t, f.h.Login, loginRequest{Email: "boot@inkyank.com", Password: "temporary-pass-1"}, nil)
	var resp sessionResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if rec.Code != http.StatusOK || !resp.PasswordChangeRequired || resp.ExpiresIn != int64(PasswordChangeTokenTTL/time.Second) {
		t.Fatalf("status %d resp %+v", rec.Code, resp)
	}
	if c, err := f.ids.Verify(resp.Token); err != nil || !c.PWC {
		t.Fatalf("want a pwc token: %+v %v", c, err)
	}
}

func TestLogin_HashCapacityExhausted_503(t *testing.T) {
	f := newAuthFixture(t, activeUser(t, "u1", "ops@inkyank.com", RoleSuperAdmin, "correct-password-1", false))
	origWait := hashSlotWait
	hashSlotWait = 20 * time.Millisecond
	t.Cleanup(func() { hashSlotWait = origWait })
	var releases []func()
	for i := 0; i < providerMaxConcurrentHashes; i++ {
		rel, err := acquireHashSlot(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, rel)
	}
	defer func() {
		for _, rel := range releases {
			rel()
		}
	}()
	if rec := postJSON(t, f.h.Login, loginRequest{Email: "ops@inkyank.com", Password: "correct-password-1"}, nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", rec.Code)
	}
}

func TestChangePassword_Flow(t *testing.T) {
	f := newAuthFixture(t, activeUser(t, "u1", "ops@inkyank.com", RoleSuperAdmin, "temporary-pass-1", true))
	actor := &Actor{UserID: "u1", Email: "ops@inkyank.com", Role: RoleSuperAdmin}
	before, _ := f.store.GetByID(context.Background(), "u1")

	if rec := postJSON(t, f.h.ChangePassword, changePasswordRequest{CurrentPassword: "not-the-current-1", NewPassword: "brand-new-pass-1"}, actor); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong current password: status %d", rec.Code)
	}
	if rec := postJSON(t, f.h.ChangePassword, changePasswordRequest{CurrentPassword: "temporary-pass-1", NewPassword: "short"}, actor); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "password_policy") {
		t.Fatalf("policy: status %d body %s", rec.Code, rec.Body)
	}

	rec := postJSON(t, f.h.ChangePassword, changePasswordRequest{CurrentPassword: "temporary-pass-1", NewPassword: "brand-new-pass-1"}, actor)
	if rec.Code != http.StatusOK {
		t.Fatalf("change: status %d body %s", rec.Code, rec.Body)
	}
	var resp sessionResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	c, err := f.ids.Verify(resp.Token)
	after, _ := f.store.GetByID(context.Background(), "u1")
	if err != nil || c.PWC || c.Gen != after.SessionGeneration || after.SessionGeneration != before.SessionGeneration+1 || after.MustChangePassword {
		t.Fatalf("after change: claims=%+v row=%+v err=%v", c, after, err)
	}
	if ok, _ := VerifyPassword(context.Background(), "brand-new-pass-1", after.PasswordHash); !ok {
		t.Fatal("new password not stored")
	}
	if f.store.auditCount(AuditPasswordChange) != 1 {
		t.Fatal("password change not audited")
	}
	for _, a := range f.store.audits {
		if strings.Contains(strings.ToLower(jsonString(a.Details)), "brand-new-pass") {
			t.Fatal("password leaked into audit details")
		}
	}
}

func TestLogout_BumpsGenerationAndAudits(t *testing.T) {
	f := newAuthFixture(t, activeUser(t, "u1", "ops@inkyank.com", RoleRelayOps, "correct-password-1", false))
	before, _ := f.store.GetByID(context.Background(), "u1")
	rec := postJSON(t, f.h.Logout, nil, &Actor{UserID: "u1", Email: "ops@inkyank.com", Role: RoleRelayOps})
	after, _ := f.store.GetByID(context.Background(), "u1")
	if rec.Code != http.StatusNoContent || after.SessionGeneration != before.SessionGeneration+1 || f.store.auditCount(AuditSessionLogout) != 1 {
		t.Fatalf("status=%d gen %d->%d audits=%d", rec.Code, before.SessionGeneration, after.SessionGeneration, f.store.auditCount(AuditSessionLogout))
	}
}

func TestLogin_RejectsMalformedBody(t *testing.T) {
	f := newAuthFixture(t)
	for _, body := range []string{"", "{", `{"email":"a@b"}`, `{"email":"a@b","password":"x","extra":1}`} {
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		rec := httptest.NewRecorder()
		f.h.Login(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %q: status %d, want 400", body, rec.Code)
		}
	}
}

func jsonString(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
