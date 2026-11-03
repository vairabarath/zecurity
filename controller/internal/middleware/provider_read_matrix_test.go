package middleware

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/yourorg/ztna/controller/internal/appmeta"
	"github.com/yourorg/ztna/controller/internal/provider"
)

// Phase R read matrix, end to end through the real RequireProvider: the role
// comes from the database row (not the token), each read action is decided
// by provider.Authz, and a tenant JWT never reaches the authorization check.
// The Phase R handlers (commit 6) put exactly this check behind each route.

var readChecks = []struct {
	action string
	check  func(*provider.Authz, provider.Actor) error
}{
	{provider.ActionRelayRead, func(a *provider.Authz, x provider.Actor) error {
		return a.CanReadRelays(x, provider.Target{Type: "relay"})
	}},
	{provider.ActionTenantRead, func(a *provider.Authz, x provider.Actor) error {
		return a.CanReadTenants(x, provider.Target{Type: "tenant"})
	}},
	{provider.ActionAuditView, func(a *provider.Authz, x provider.Actor) error { return a.CanViewProviderAudit(x) }},
	{provider.ActionCertRead, func(a *provider.Authz, x provider.Actor) error { return a.CanReadCertificates(x) }},
}

// serveRead runs RequireProvider followed by one read check, mapping
// ErrForbidden to 403 like the provider handlers do. reached reports whether
// the authorization check ran at all.
func serveRead(t *testing.T, f *requireProviderFixture, authHeader string, check func(*provider.Authz, provider.Actor) error) (code int, reached bool) {
	t.Helper()
	authz := provider.NewAuthz()
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		actor, ok := provider.ActorFromContext(r.Context())
		if !ok {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if err := check(authz, actor); err != nil {
			if errors.Is(err, provider.ErrForbidden) {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodGet, "/provider/read", nil)
	req.Header.Set("Authorization", authHeader)
	rec := httptest.NewRecorder()
	RequireProvider(f.ids, f.users)(next).ServeHTTP(rec, req)
	return rec.Code, reached
}

func TestReadMatrix_ThroughRequireProvider(t *testing.T) {
	f := newRequireProviderFixture(t)
	f.users.users["ops"] = &provider.ProviderUser{ID: "ops", Email: "relay@inkyank.com", Role: provider.RoleRelayOps, SessionGeneration: 1}
	f.users.users["odd"] = &provider.ProviderUser{ID: "odd", Email: "odd@inkyank.com", Role: "admin", SessionGeneration: 1}

	// Each token claims super-admin; only the database row decides.
	tokenFor := func(id, email string, gen int64) string {
		return f.token(t, provider.ProviderUser{ID: id, Email: email, Role: provider.RoleSuperAdmin, SessionGeneration: gen}, false)
	}
	callers := []struct {
		name    string
		header  string
		allowed map[string]bool
	}{
		{"super-admin", "Bearer " + tokenFor("u1", "ops@inkyank.com", 3), map[string]bool{
			provider.ActionRelayRead: true, provider.ActionTenantRead: true,
			provider.ActionAuditView: true, provider.ActionCertRead: true,
		}},
		{"relay-ops (token claims super-admin)", "Bearer " + tokenFor("ops", "relay@inkyank.com", 1), map[string]bool{
			provider.ActionRelayRead: true,
		}},
		{"unknown role in DB", "Bearer " + tokenFor("odd", "odd@inkyank.com", 1), map[string]bool{}},
	}
	for _, c := range callers {
		for _, rc := range readChecks {
			code, reached := serveRead(t, f, c.header, rc.check)
			want := http.StatusForbidden
			if c.allowed[rc.action] {
				want = http.StatusOK
			}
			if code != want || !reached {
				t.Errorf("%s, %s: code=%d reached=%v, want %d", c.name, rc.action, code, reached, want)
			}
		}
	}
}

// A tenant JWT (same secret, tenant issuer) is rejected with 401 by
// RequireProvider for every read action, before any authorization check.
func TestReadMatrix_TenantJWT401(t *testing.T) {
	f := newRequireProviderFixture(t)
	now := time.Now()
	tenantTok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, Claims{
		TenantID: "tenant-123", Role: "admin", Email: "ops@inkyank.com",
		RegisteredClaims: jwt.RegisteredClaims{
			Subject: "u1", Issuer: appmeta.ControllerIssuer,
			IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(time.Minute)),
		},
	}).SignedString([]byte(testSecret))
	if err != nil {
		t.Fatal(err)
	}
	for _, rc := range readChecks {
		code, reached := serveRead(t, f, "Bearer "+tenantTok, rc.check)
		if code != http.StatusUnauthorized || reached {
			t.Errorf("%s: tenant JWT code=%d reached=%v, want 401 and not reached", rc.action, code, reached)
		}
	}
}
