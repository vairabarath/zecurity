package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/yourorg/ztna/controller/internal/appmeta"
	"github.com/yourorg/ztna/controller/internal/provider"
)

// testSecret is deliberately used as BOTH the tenant secret and the provider
// key: the walls below must hold even in that worst case.
const testSecret = "isolation-test-secret-0123456789abcdef"

// A provider token must be REJECTED by the tenant AuthMiddleware: it carries no
// tenant_id, and AuthMiddleware requires one. This blocks a provider identity
// from ever reaching a tenant (WorkspaceGuard-protected) handler.
func TestTenantMiddlewareRejectsProviderToken(t *testing.T) {
	ids, err := provider.NewIdentityService(testSecret)
	if err != nil {
		t.Fatal(err)
	}
	tok, _, err := ids.IssueSession(&provider.ProviderUser{ID: "puid-1", Email: "ops@corp.com", Role: provider.RoleSuperAdmin, SessionGeneration: 1},
		[]string{provider.AMRPassword}, false)
	if err != nil {
		t.Fatal(err)
	}

	nextCalled := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { nextCalled = true })
	h := AuthMiddleware(testSecret)(next)

	req := httptest.NewRequest(http.MethodGet, "/graphql", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("provider token accepted by tenant middleware: got %d, want 401", rec.Code)
	}
	if nextCalled {
		t.Fatal("tenant handler ran for a provider token")
	}
}

// A tenant token must be REJECTED by VerifyProviderToken: it has no
// aud=provider, and VerifyProviderToken enforces that audience. This blocks a
// tenant identity from ever passing RequireProvider.
func TestProviderVerifyRejectsTenantToken(t *testing.T) {
	now := time.Now()
	tenantTok := jwt.NewWithClaims(jwt.SigningMethodHS256, Claims{
		TenantID: "tenant-123",
		Role:     "admin",
		Email:    "user@tenant.com",
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "tenant-user-1",
			Issuer:    appmeta.ControllerIssuer, // same issuer + secret …
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Minute)),
			// … but deliberately NO Audience: this is the wall.
		},
	})
	signed, err := tenantTok.SignedString([]byte(testSecret))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := provider.VerifyProviderToken([]byte(testSecret), signed); err == nil {
		t.Fatal("tenant token accepted by VerifyProviderToken — audience wall is broken")
	}
}

// ── RequireProvider (Sprint 21 Phase H) ─────────────────────────────────────

// memProviderUsers is a GetByID fake; err overrides lookups.
type memProviderUsers struct {
	users map[string]*provider.ProviderUser
	err   error
}

func (m *memProviderUsers) GetByID(_ context.Context, id string) (*provider.ProviderUser, error) {
	if m.err != nil {
		return nil, m.err
	}
	u, ok := m.users[id]
	if !ok {
		return nil, provider.ErrProviderUserNotFound
	}
	c := *u
	return &c, nil
}

type requireProviderFixture struct {
	ids   *provider.IdentityService
	users *memProviderUsers
}

func newRequireProviderFixture(t *testing.T) *requireProviderFixture {
	t.Helper()
	ids, err := provider.NewIdentityService(testSecret)
	if err != nil {
		t.Fatal(err)
	}
	return &requireProviderFixture{ids: ids, users: &memProviderUsers{users: map[string]*provider.ProviderUser{
		"u1": {ID: "u1", Email: "ops@inkyank.com", Role: provider.RoleSuperAdmin, SessionGeneration: 3},
	}}}
}

func (f *requireProviderFixture) token(t *testing.T, u provider.ProviderUser, pwc bool) string {
	t.Helper()
	tok, _, err := f.ids.IssueSession(&u, []string{provider.AMRPassword}, pwc)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// serve runs RequireProvider with a handler that records the injected Actor.
func (f *requireProviderFixture) serve(t *testing.T, authz string, opts ...ProviderOption) (int, string, *provider.Actor) {
	t.Helper()
	var got *provider.Actor
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a, ok := provider.ActorFromContext(r.Context()); ok {
			got = &a
		}
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodGet, "/provider/me", nil)
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	rec := httptest.NewRecorder()
	RequireProvider(f.ids, f.users, opts...)(next).ServeHTTP(rec, req)
	return rec.Code, rec.Body.String(), got
}

func TestRequireProvider_ValidTokenInjectsActorFromDB(t *testing.T) {
	f := newRequireProviderFixture(t)
	// The token claims relay-ops, the DB says super-admin: the DB wins.
	tok := f.token(t, provider.ProviderUser{ID: "u1", Email: "ops@inkyank.com", Role: provider.RoleRelayOps, SessionGeneration: 3}, false)
	code, body, actor := f.serve(t, "Bearer "+tok)
	if code != http.StatusOK || actor == nil || actor.UserID != "u1" || actor.Role != provider.RoleSuperAdmin {
		t.Fatalf("code=%d body=%s actor=%+v", code, body, actor)
	}
}

func TestRequireProvider_RejectsBadHeaders(t *testing.T) {
	f := newRequireProviderFixture(t)
	for _, h := range []string{"", "Basic abc", "Bearer ", "Bearer not-a-jwt"} {
		if code, _, actor := f.serve(t, h); code != http.StatusUnauthorized || actor != nil {
			t.Errorf("header %q: code=%d", h, code)
		}
	}
}

// A tenant token signed with the very same secret still fails (issuer + aud).
func TestRequireProvider_RejectsTenantToken(t *testing.T) {
	f := newRequireProviderFixture(t)
	now := time.Now()
	tenantTok, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, Claims{
		TenantID: "tenant-123", Role: "admin", Email: "ops@inkyank.com",
		RegisteredClaims: jwt.RegisteredClaims{
			Subject: "u1", Issuer: appmeta.ControllerIssuer,
			IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(time.Minute)),
		},
	}).SignedString([]byte(testSecret))
	if code, _, _ := f.serve(t, "Bearer "+tenantTok); code != http.StatusUnauthorized {
		t.Fatalf("tenant token: code=%d, want 401", code)
	}
}

func TestRequireProvider_GenerationMismatch401(t *testing.T) {
	f := newRequireProviderFixture(t)
	tok := f.token(t, provider.ProviderUser{ID: "u1", Email: "ops@inkyank.com", Role: provider.RoleSuperAdmin, SessionGeneration: 3}, false)
	f.users.users["u1"].SessionGeneration = 4 // logout / password change / disable happened
	code, body, actor := f.serve(t, "Bearer "+tok)
	if code != http.StatusUnauthorized || actor != nil || !strings.Contains(body, "provider session revoked") {
		t.Fatalf("code=%d body=%s", code, body)
	}
}

func TestRequireProvider_DisabledOrUnknownUser403(t *testing.T) {
	f := newRequireProviderFixture(t)
	tok := f.token(t, provider.ProviderUser{ID: "u1", Email: "ops@inkyank.com", Role: provider.RoleSuperAdmin, SessionGeneration: 3}, false)
	now := time.Now()
	f.users.users["u1"].DisabledAt = &now
	if code, _, _ := f.serve(t, "Bearer "+tok); code != http.StatusForbidden {
		t.Fatalf("disabled: code=%d, want 403", code)
	}
	ghost := f.token(t, provider.ProviderUser{ID: "ghost", Email: "ghost@inkyank.com", Role: provider.RoleSuperAdmin, SessionGeneration: 1}, false)
	if code, _, _ := f.serve(t, "Bearer "+ghost); code != http.StatusForbidden {
		t.Fatalf("unknown user: code=%d, want 403", code)
	}
}

func TestRequireProvider_EmailMismatch401(t *testing.T) {
	f := newRequireProviderFixture(t)
	tok := f.token(t, provider.ProviderUser{ID: "u1", Email: "old@inkyank.com", Role: provider.RoleSuperAdmin, SessionGeneration: 3}, false)
	if code, _, _ := f.serve(t, "Bearer "+tok); code != http.StatusUnauthorized {
		t.Fatalf("code=%d, want 401", code)
	}
}

// A password-change-only token works only where explicitly allowed.
func TestRequireProvider_PasswordChangeTokenScope(t *testing.T) {
	f := newRequireProviderFixture(t)
	pwc := f.token(t, provider.ProviderUser{ID: "u1", Email: "ops@inkyank.com", Role: provider.RoleSuperAdmin, SessionGeneration: 3}, true)
	code, body, actor := f.serve(t, "Bearer "+pwc)
	if code != http.StatusForbidden || actor != nil || !strings.Contains(body, "password_change_required") {
		t.Fatalf("default route: code=%d body=%s", code, body)
	}
	if code, _, actor := f.serve(t, "Bearer "+pwc, AllowPasswordChangeToken()); code != http.StatusOK || actor == nil {
		t.Fatalf("password route: code=%d", code)
	}
}

func TestRequireProvider_LookupError500(t *testing.T) {
	f := newRequireProviderFixture(t)
	tok := f.token(t, provider.ProviderUser{ID: "u1", Email: "ops@inkyank.com", Role: provider.RoleSuperAdmin, SessionGeneration: 3}, false)
	f.users.err = errors.New("db down")
	if code, _, _ := f.serve(t, "Bearer "+tok); code != http.StatusInternalServerError {
		t.Fatalf("code=%d, want 500", code)
	}
}
