package resolvers

// Coverage for the create-time OIDC discovery validation added to
// createIdpConnection: a connection whose issuer does not serve a valid OIDC
// discovery document must be REFUSED and must not be persisted, so the admin UI
// can never present an unverified configuration as a saved provider.
//
// Two layers:
//
//   - validateOIDCDiscovery unit tests, which need no database and therefore
//     always run. These cover the reachability/validity matrix and the
//     user-safety of the returned error.
//   - CreateIdpConnection integration tests asserting the row is genuinely
//     absent, which reuse the live-Postgres harness and skip cleanly when
//     PKI_TEST_DATABASE_URL is unset.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/yourorg/ztna/controller/graph"
	"github.com/yourorg/ztna/controller/internal/apperr"
)

// seedWorkspaceForCreateTest inserts the workspace row identity_connections
// requires by FK, so the create path is exercised for a legitimate workspace
// rather than incidentally failing on a constraint. Local to this file so it
// does not depend on the shared harness's seeding helpers.
func seedWorkspaceForCreateTest(t *testing.T, h *scimEnableHarness) string {
	t.Helper()
	ws := uuid.NewString()
	if _, err := h.pool.Exec(context.Background(),
		`INSERT INTO workspaces (id, slug, name, status, trust_domain)
		 VALUES ($1,$2,$3,'active',$4)
		 ON CONFLICT (id) DO NOTHING`,
		ws, "idpcreate-"+ws[:8], "IdP Create Test Corp", "td-idpcreate-"+ws[:8],
	); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	return ws
}

// newValidDiscoveryServer serves a discovery document advertising itself as the
// issuer — the shape a correctly configured Okta/OIDC tenant returns.
func newValidDiscoveryServer(t *testing.T) *httptest.Server {
	t.Helper()
	var issuer string
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":                 issuer,
			"authorization_endpoint": issuer + "/authorize",
			"token_endpoint":         issuer + "/token",
			"jwks_uri":               issuer + "/jwks",
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	issuer = srv.URL
	return srv
}

func TestValidateOIDCDiscovery_ValidIssuerPasses(t *testing.T) {
	srv := newValidDiscoveryServer(t)

	if err := validateOIDCDiscovery(context.Background(), "okta", srv.URL, "", ""); err != nil {
		t.Fatalf("expected a valid issuer to pass validation, got %v", err)
	}
}

func TestValidateOIDCDiscovery_UnreachableIssuerFails(t *testing.T) {
	err := validateOIDCDiscovery(context.Background(), "okta", "https://acme-not-a-real-org.invalid", "", "")
	if err == nil {
		t.Fatal("expected an unreachable issuer to fail validation")
	}
}

func TestValidateOIDCDiscovery_InvalidDocumentFails(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		// A 200 that is not a discovery document at all — e.g. an Okta org URL
		// that serves an HTML sign-in page.
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><body>sign in</body></html>"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	if err := validateOIDCDiscovery(context.Background(), "okta", srv.URL, "", ""); err == nil {
		t.Fatal("expected an invalid discovery document to fail validation")
	}
}

func TestValidateOIDCDiscovery_IssuerMismatchFails(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":                 "https://someone-else.example.com",
			"authorization_endpoint": "https://someone-else.example.com/authorize",
			"token_endpoint":         "https://someone-else.example.com/token",
			"jwks_uri":               "https://someone-else.example.com/jwks",
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	err := validateOIDCDiscovery(context.Background(), "okta", srv.URL, "", "")
	if err == nil {
		t.Fatal("expected a discovery issuer mismatch to fail validation")
	}
	if !strings.Contains(err.Error(), "issuer mismatch") {
		t.Fatalf("expected the mismatch to be explained to the admin, got %q", err.Error())
	}
}

// The failure must reach the admin, not be masked. ErrorPresenter is fail-closed:
// anything that is not an *apperr.UserError (or *gqlerror.Error) becomes
// "an unexpected error occurred", which would leave the admin with no idea why
// the connection was refused.
func TestValidateOIDCDiscovery_ErrorIsUserSafeAndActionable(t *testing.T) {
	err := validateOIDCDiscovery(context.Background(), "okta", "https://acme-not-a-real-org.invalid", "", "")
	if err == nil {
		t.Fatal("expected an error")
	}

	var ue *apperr.UserError
	if !errors.As(err, &ue) {
		t.Fatalf("expected an *apperr.UserError so ErrorPresenter surfaces it verbatim, got %T", err)
	}

	msg := err.Error()
	// It must say the connection was not created, and must NOT imply the
	// credentials were checked.
	if !strings.Contains(msg, "NOT created") {
		t.Fatalf("expected the message to state the connection was not created, got %q", msg)
	}
	for _, forbidden := range []string{"credentials verified", "credentials are valid", "client secret is valid"} {
		if strings.Contains(strings.ToLower(msg), forbidden) {
			t.Fatalf("message must not claim credential validation, got %q", msg)
		}
	}
}

// Structural credential-safety: validateOIDCDiscovery never receives the client
// secret, so no error it builds can contain one. Asserted here against a real
// failing probe with a secret-shaped value present nowhere in the call.
func TestValidateOIDCDiscovery_ErrorCarriesNoCredentialMaterial(t *testing.T) {
	const secret = "super-secret-value-must-never-appear"

	err := validateOIDCDiscovery(context.Background(), "okta", "https://acme-not-a-real-org.invalid", "", "")
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatal("discovery validation error carried credential material")
	}
	if strings.Contains(strings.ToLower(err.Error()), "authorization:") {
		t.Fatalf("discovery validation error carried an Authorization header, got %q", err.Error())
	}
}

// --- integration: the row must genuinely not exist -------------------------

func TestCreateIdpConnection_RefusesAndDoesNotPersistOnDiscoveryFailure(t *testing.T) {
	h := newScimEnableHarness(t) // skips when PKI_TEST_DATABASE_URL is unset
	defer h.teardown()
	mr := h.mutationResolver()

	const badIssuer = "https://acme-not-a-real-org.invalid"
	ws := seedWorkspaceForCreateTest(t, h)

	_, err := mr.CreateIdpConnection(h.ctxFor(ws), graph.CreateIdpConnectionInput{
		Provider:     "okta",
		DisplayName:  "Corporate Okta",
		Issuer:       badIssuer,
		ClientID:     "client-abc",
		ClientSecret: "super-secret-value-must-never-appear",
	})
	if err == nil {
		t.Fatal("expected CreateIdpConnection to refuse an unreachable issuer")
	}

	var ue *apperr.UserError
	if !errors.As(err, &ue) {
		t.Fatalf("expected a user-safe error, got %T: %v", err, err)
	}

	// The invariant that matters: nothing was written.
	var n int
	if qerr := h.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM identity_connections WHERE issuer = $1`, badIssuer).Scan(&n); qerr != nil {
		t.Fatalf("count connections: %v", qerr)
	}
	if n != 0 {
		t.Fatalf("PERSISTENCE REGRESSION: expected no connection row for a failed discovery check, found %d", n)
	}
}

func TestCreateIdpConnection_PersistsWhenDiscoverySucceeds(t *testing.T) {
	h := newScimEnableHarness(t) // skips when PKI_TEST_DATABASE_URL is unset
	defer h.teardown()
	mr := h.mutationResolver()

	issuer := h.startDiscoveryFixture()
	ws := seedWorkspaceForCreateTest(t, h)

	// ClientSecret is deliberately empty: the shared harness builds its idp.Store
	// with a nil encryptor (its other tests seed rows via raw SQL), and
	// CreateWorkspaceConnection only reaches the encryptor for a NON-empty
	// secret. Supplying one would require a full pki.Service stub, which is
	// beside the point of this test — that a VALID discovery document lets the
	// create proceed to a persisted row. Secret handling is covered separately
	// (the store's own encryption tests, and the no-credential-material tests
	// above).
	created, err := mr.CreateIdpConnection(h.ctxFor(ws), graph.CreateIdpConnectionInput{
		Provider:    "okta",
		DisplayName: "Corporate Okta",
		Issuer:      issuer,
		ClientID:    "client-abc",
	})
	if err != nil {
		t.Fatalf("expected creation to succeed against a valid discovery fixture: %v", err)
	}
	if created == nil {
		t.Fatal("expected a created connection")
	}
	if created.Issuer != issuer {
		t.Fatalf("expected issuer %q, got %q", issuer, created.Issuer)
	}

	// The row is really there.
	var n int
	if qerr := h.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM identity_connections WHERE issuer = $1`, issuer).Scan(&n); qerr != nil {
		t.Fatalf("count connections: %v", qerr)
	}
	if n != 1 {
		t.Fatalf("expected exactly 1 persisted connection for a valid issuer, found %d", n)
	}

	// The GraphQL view must never carry the client secret in any form (ADR-024).
	// Asserted against the marshalled response so that ADDING such a field to
	// the type or its mapping fails here.
	blob, merr := json.Marshal(created)
	if merr != nil {
		t.Fatalf("marshal created connection: %v", merr)
	}
	if strings.Contains(strings.ToLower(string(blob)), "secret") {
		t.Fatalf("the created connection response exposed a secret-bearing field: %s", string(blob))
	}
}

// --- update: a discoveryUrl override is validated too ----------------------

// The same false-verified state was reachable through updateIdpConnection: a
// connection that passed the check at create time could be repointed at an
// unreachable discovery override.
func TestUpdateIdpConnection_RefusesUnreachableDiscoveryURLOverride(t *testing.T) {
	h := newScimEnableHarness(t) // skips when PKI_TEST_DATABASE_URL is unset
	defer h.teardown()
	mr := h.mutationResolver()

	issuer := h.startDiscoveryFixture()
	ws, connID := h.seedConnection(issuer)

	bad := "https://acme-not-a-real-org.invalid/.well-known/openid-configuration"
	_, err := mr.UpdateIdpConnection(h.ctxFor(ws), connID, graph.UpdateIdpConnectionInput{
		DiscoveryURL: &bad,
	})
	if err == nil {
		t.Fatal("expected UpdateIdpConnection to refuse an unreachable discoveryUrl override")
	}

	var ue *apperr.UserError
	if !errors.As(err, &ue) {
		t.Fatalf("expected a user-safe error, got %T: %v", err, err)
	}

	// The override must not have been written.
	var stored *string
	if qerr := h.pool.QueryRow(context.Background(),
		`SELECT discovery_url FROM identity_connections WHERE id = $1`, connID).Scan(&stored); qerr != nil {
		t.Fatalf("read discovery_url: %v", qerr)
	}
	if stored != nil {
		t.Fatalf("PERSISTENCE REGRESSION: expected discovery_url to remain unset, got %q", *stored)
	}
}

// Guard against over-validating: updates that do NOT touch discoveryUrl must
// still succeed even when the issuer is unreachable, so routine edits (renaming
// a connection, rotating a secret) never depend on the IdP being up.
func TestUpdateIdpConnection_NonDiscoveryFieldsSkipTheProbe(t *testing.T) {
	h := newScimEnableHarness(t) // skips when PKI_TEST_DATABASE_URL is unset
	defer h.teardown()
	mr := h.mutationResolver()

	// Deliberately unreachable issuer: if a probe ran, this would fail.
	ws, connID := h.seedConnection("https://acme-not-a-real-org.invalid")

	newName := "Renamed Okta"
	updated, err := mr.UpdateIdpConnection(h.ctxFor(ws), connID, graph.UpdateIdpConnectionInput{
		DisplayName: &newName,
	})
	if err != nil {
		t.Fatalf("expected a displayName-only update to skip the discovery probe, got %v", err)
	}
	if updated.DisplayName != newName {
		t.Fatalf("expected displayName %q, got %q", newName, updated.DisplayName)
	}
}

// Tenant isolation: another workspace's connection must be invisible, and must
// not be probed on its behalf.
func TestUpdateIdpConnection_CrossWorkspaceDiscoveryUpdateRejected(t *testing.T) {
	h := newScimEnableHarness(t) // skips when PKI_TEST_DATABASE_URL is unset
	defer h.teardown()
	mr := h.mutationResolver()

	issuer := h.startDiscoveryFixture()
	_, connID := h.seedConnection(issuer)
	otherWS := seedWorkspaceForCreateTest(t, h)

	override := issuer + "/.well-known/openid-configuration"
	_, err := mr.UpdateIdpConnection(h.ctxFor(otherWS), connID, graph.UpdateIdpConnectionInput{
		DiscoveryURL: &override,
	})
	if err == nil {
		t.Fatal("expected a cross-workspace update to be rejected")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected a not-found style rejection that reveals nothing, got %q", err.Error())
	}
}
