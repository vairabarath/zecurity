package relay

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yourorg/ztna/controller/internal/provider"
)

// A non-POST request is rejected before any provider/authz logic runs.
func TestAdminCreateRejectsNonPost(t *testing.T) {
	h := &AdminHandler{Authz: provider.NewAuthz()}
	req := httptest.NewRequest(http.MethodGet, "/provider/relays", nil)
	rec := httptest.NewRecorder()

	h.Create(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: got %d, want 405", rec.Code)
	}
}

// No provider Actor in context (handler wired without RequireProvider) must fail
// CLOSED with 500 — never proceed as an anonymous request.
func TestAdminCreateRequiresProviderActor(t *testing.T) {
	h := &AdminHandler{Authz: provider.NewAuthz()}
	req := httptest.NewRequest(http.MethodPost, "/provider/relays", strings.NewReader(`{"name":"r1"}`))
	rec := httptest.NewRecorder()

	h.Create(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("missing actor: got %d, want 500", rec.Code)
	}
}

// An actor whose role is not permitted the relay.* namespace is blocked by the
// Authz chokepoint BEFORE any relay/token work. Store and Redis are left nil on
// purpose: if the gate failed to block, the handler would reach h.Store and panic
// — so a clean 403 proves the chokepoint stopped it first.
func TestAdminCreateForbiddenRole(t *testing.T) {
	h := &AdminHandler{Authz: provider.NewAuthz()} // Store/Redis nil on purpose
	actor := provider.Actor{UserID: "p1", Email: "x@corp.com", Role: "no-such-role"}
	req := httptest.NewRequest(http.MethodPost, "/provider/relays", strings.NewReader(`{"name":"r1"}`))
	req = req.WithContext(provider.WithActor(req.Context(), actor))
	rec := httptest.NewRecorder()

	h.Create(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("forbidden role: got %d, want 403", rec.Code)
	}
}
