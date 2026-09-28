package middleware

import (
	"net/http"
	"net/http/httptest"
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
