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

// A non-DELETE method is rejected before any provider/authz logic runs.
func TestAdminDeleteRejectsNonDelete(t *testing.T) {
	h := &AdminHandler{Authz: provider.NewAuthz()}
	req := httptest.NewRequest(http.MethodGet, "/provider/relays/abc", nil)
	rec := httptest.NewRecorder()

	h.Delete(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: got %d, want 405", rec.Code)
	}
}

// No provider Actor in context must fail CLOSED with 500 — never an anonymous delete.
func TestAdminDeleteRequiresProviderActor(t *testing.T) {
	h := &AdminHandler{Authz: provider.NewAuthz()}
	req := httptest.NewRequest(http.MethodDelete, "/provider/relays/abc", nil)
	req.SetPathValue("id", "abc")
	rec := httptest.NewRecorder()

	h.Delete(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("missing actor: got %d, want 500", rec.Code)
	}
}

// A disallowed role is blocked by CanDeleteRelay BEFORE any store work. Store is
// nil on purpose: reaching it (a failed gate) would panic, so a clean 403 proves
// the chokepoint stopped it first.
func TestAdminDeleteForbiddenRole(t *testing.T) {
	h := &AdminHandler{Authz: provider.NewAuthz()} // Store/ProviderStore nil on purpose
	actor := provider.Actor{UserID: "p1", Email: "x@corp.com", Role: "no-such-role"}
	req := httptest.NewRequest(http.MethodDelete, "/provider/relays/abc", nil)
	req.SetPathValue("id", "abc")
	req = req.WithContext(provider.WithActor(req.Context(), actor))
	rec := httptest.NewRecorder()

	h.Delete(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("forbidden role: got %d, want 403", rec.Code)
	}
}

// A non-POST request to Revoke is rejected before any provider/authz logic runs.
func TestAdminRevokeRejectsNonPost(t *testing.T) {
	h := &AdminHandler{Authz: provider.NewAuthz()}
	req := httptest.NewRequest(http.MethodGet, "/provider/relays/abc/revoke", nil)
	rec := httptest.NewRecorder()

	h.Revoke(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: got %d, want 405", rec.Code)
	}
}

// No provider Actor in context must fail CLOSED with 500 — never an anonymous revoke.
func TestAdminRevokeRequiresProviderActor(t *testing.T) {
	h := &AdminHandler{Authz: provider.NewAuthz()}
	req := httptest.NewRequest(http.MethodPost, "/provider/relays/abc/revoke", nil)
	req.SetPathValue("id", "abc")
	rec := httptest.NewRecorder()

	h.Revoke(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("missing actor: got %d, want 500", rec.Code)
	}
}

// A disallowed role is blocked by CanRevokeRelay BEFORE any store work. Store is
// nil on purpose: reaching it (a failed gate) would panic, so a clean 403 proves
// the chokepoint stopped it first.
func TestAdminRevokeForbiddenRole(t *testing.T) {
	h := &AdminHandler{Authz: provider.NewAuthz()} // Store/ProviderStore nil on purpose
	actor := provider.Actor{UserID: "p1", Email: "x@corp.com", Role: "no-such-role"}
	req := httptest.NewRequest(http.MethodPost, "/provider/relays/abc/revoke", nil)
	req.SetPathValue("id", "abc")
	req = req.WithContext(provider.WithActor(req.Context(), actor))
	rec := httptest.NewRecorder()

	h.Revoke(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("forbidden role: got %d, want 403", rec.Code)
	}
}

// Missing relay id in the path is rejected before any store work.
func TestAdminRevokeRequiresRelayID(t *testing.T) {
	h := &AdminHandler{Authz: provider.NewAuthz()}
	actor := provider.Actor{UserID: "p1", Email: "x@corp.com", Role: provider.RoleRelayOps}
	req := httptest.NewRequest(http.MethodPost, "/provider/relays//revoke", nil)
	req.SetPathValue("id", "")
	req = req.WithContext(provider.WithActor(req.Context(), actor))
	rec := httptest.NewRecorder()

	h.Revoke(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing id: got %d, want 400", rec.Code)
	}
}
