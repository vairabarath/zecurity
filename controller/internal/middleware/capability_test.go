package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/yourorg/ztna/controller/internal/rbac"
	"github.com/yourorg/ztna/controller/internal/tenant"
)

type fakeRoles struct {
	role string
	err  error
}

func (f fakeRoles) Role(context.Context, string, string) (string, error) { return f.role, f.err }

// The three REST routes (invitations: MEMBERS_ADMIN; connector/shield
// tokens: NETWORK_WRITE) use the live role, never the JWT claim.
func TestRequireCapability(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if role, _ := rbac.RoleFrom(r.Context()); role == "" {
			t.Error("live role not in context")
		}
		w.WriteHeader(http.StatusNoContent)
	})
	for _, tc := range []struct {
		name   string
		roles  fakeRoles
		cap    rbac.Capability
		noAuth bool
		want   int
	}{
		{"admin invites", fakeRoles{role: "admin"}, rbac.MembersAdmin, false, 204},
		{"devops token", fakeRoles{role: "devops"}, rbac.NetworkWrite, false, 204},
		{"devops can't invite", fakeRoles{role: "devops"}, rbac.MembersAdmin, false, 403},
		{"security no token", fakeRoles{role: "security"}, rbac.NetworkWrite, false, 403},
		{"viewer no token", fakeRoles{role: "viewer"}, rbac.NetworkWrite, false, 403},
		{"member", fakeRoles{role: "member"}, rbac.NetworkWrite, false, 403},
		{"suspended", fakeRoles{err: rbac.ErrNoAccess}, rbac.NetworkWrite, false, 403},
		{"lookup fails", fakeRoles{err: errors.New("db down")}, rbac.NetworkWrite, false, 403},
		{"no identity", fakeRoles{role: "admin"}, rbac.NetworkWrite, true, 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/api/connectors/x/token", nil)
			if !tc.noAuth {
				// The JWT claims admin; only the live role may count.
				req = req.WithContext(tenant.Set(req.Context(), tenant.TenantContext{TenantID: "t", UserID: "u", Role: "admin"}))
			}
			rec := httptest.NewRecorder()
			RequireCapability(tc.roles, tc.cap)(ok).ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status %d, want %d", rec.Code, tc.want)
			}
		})
	}
}
