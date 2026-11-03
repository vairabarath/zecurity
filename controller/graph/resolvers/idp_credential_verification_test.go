package resolvers

// Behavioural coverage for the credential-verification gate on the admin IdP
// API. The unit-level classification lives in
// internal/auth/providers/oidc_verify_credentials_test.go; these tests prove the
// RESOLVER contract:
//
//   - a rejected credential pair (the swapped client-ID/secret case) blocks
//     create and persists NOTHING;
//   - a credential-changing update is verified and, when refused, leaves the row
//     completely untouched — no partial update;
//   - a metadata-only update is NOT credential-probed;
//   - testIdpConnection reports Ok=false rather than claiming success.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yourorg/ztna/controller/graph"
)

// startRejectingDiscoveryFixture serves valid discovery but REJECTS the
// credential probe, i.e. an IdP that is reachable and correctly configured while
// the supplied client credentials are wrong.
func startRejectingDiscoveryFixture(t *testing.T) string {
	t.Helper()
	var issuerURL string
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":                 issuerURL,
			"authorization_endpoint": issuerURL + "/authorize",
			"token_endpoint":         issuerURL + "/token",
			"jwks_uri":               issuerURL + "/jwks",
		})
	})
	registerRejectingCredentialProbeToken(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	issuerURL = srv.URL
	return issuerURL
}

// The regression this whole gate exists for: swapping the client ID and client
// secret used to produce a happily-created connection that could not
// authenticate anyone, surfacing only as an opaque IdP error at first login.
func TestCreateIdpConnection_RejectedCredentialsPersistNothing(t *testing.T) {
	h := newScimEnableHarness(t)
	defer h.teardown()
	mr := h.mutationResolver()

	issuer := startRejectingDiscoveryFixture(t)
	ws := seedWorkspaceForCreateTest(t, h)

	_, err := mr.CreateIdpConnection(h.ctxFor(ws), graph.CreateIdpConnectionInput{
		Provider:    "okta",
		DisplayName: "Swapped Okta",
		Issuer:      issuer,
		// The swap: the 64-char secret pasted into the client ID field.
		ClientID:     "YsDqypvxGeI1KEgfHxHOTjwywlxpyiu7I9WMw5rYz-HB411JFZYM2cpViUeIGx_-",
		ClientSecret: "0oa8x9k2mCvbNqL4x697",
	})
	if err == nil {
		t.Fatal("expected creation to be refused when the IdP rejects the credentials")
	}
	if !strings.Contains(err.Error(), "invalid_client") {
		t.Fatalf("the refusal should name the IdP's error code, got: %v", err)
	}
	// The message must coach the actual mistake, not just fail.
	if !strings.Contains(err.Error(), "each other's field") {
		t.Fatalf("the refusal should mention the swap possibility, got: %v", err)
	}

	var n int
	if qerr := h.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM identity_connections WHERE issuer = $1`, issuer).Scan(&n); qerr != nil {
		t.Fatalf("count connections: %v", qerr)
	}
	if n != 0 {
		t.Fatalf("PERSISTENCE REGRESSION: unverified credentials must not be saved, found %d row(s)", n)
	}
}

// A refused credential change must not partially apply — neither the credential
// nor anything else in the same mutation.
func TestUpdateIdpConnection_RejectedCredentialChangeLeavesRowUntouched(t *testing.T) {
	h := newScimEnableHarness(t)
	defer h.teardown()
	mr := h.mutationResolver()

	// Seeded against a fixture that REFUSES the probe, so the update's
	// verification of the new pair fails.
	issuer := startRejectingDiscoveryFixture(t)
	ws, connID := h.seedConnection(issuer)

	var beforeID, beforeName string
	if err := h.pool.QueryRow(context.Background(),
		`SELECT client_id, display_name FROM identity_connections WHERE id = $1`, connID,
	).Scan(&beforeID, &beforeName); err != nil {
		t.Fatalf("read row before: %v", err)
	}

	newID, newSecret, newName := "rotated-client-id", "rotated-secret", "Renamed In The Same Call"
	_, err := mr.UpdateIdpConnection(h.ctxFor(ws), connID, graph.UpdateIdpConnectionInput{
		ClientID:     &newID,
		ClientSecret: &newSecret,
		DisplayName:  &newName,
	})
	if err == nil {
		t.Fatal("expected the credential-changing update to be refused")
	}

	var afterID, afterName string
	if qerr := h.pool.QueryRow(context.Background(),
		`SELECT client_id, display_name FROM identity_connections WHERE id = $1`, connID,
	).Scan(&afterID, &afterName); qerr != nil {
		t.Fatalf("read row after: %v", qerr)
	}
	if afterID != beforeID {
		t.Fatalf("client_id changed despite a refused verification: %q -> %q", beforeID, afterID)
	}
	// The rename rode along in the refused mutation and must not have applied.
	if afterName != beforeName {
		t.Fatalf("PARTIAL UPDATE: display_name changed despite refusal: %q -> %q", beforeName, afterName)
	}
}

// A rename must not pay for a credential probe — and must still succeed even
// against an IdP that would reject one.
func TestUpdateIdpConnection_MetadataOnlyIsNotCredentialProbed(t *testing.T) {
	h := newScimEnableHarness(t)
	defer h.teardown()
	mr := h.mutationResolver()

	var probed bool
	var issuerURL string
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":                 issuerURL,
			"authorization_endpoint": issuerURL + "/authorize",
			"token_endpoint":         issuerURL + "/token",
			"jwks_uri":               issuerURL + "/jwks",
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		probed = true
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"errorCode":"invalid_client"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	issuerURL = srv.URL

	ws, connID := h.seedConnection(issuerURL)

	newName := "Just A Rename"
	if _, err := mr.UpdateIdpConnection(h.ctxFor(ws), connID, graph.UpdateIdpConnectionInput{
		DisplayName: &newName,
	}); err != nil {
		t.Fatalf("a metadata-only update must not be gated on credentials: %v", err)
	}
	if probed {
		t.Fatal("the token endpoint was probed for an update that changed no credential")
	}

	var got string
	if err := h.pool.QueryRow(context.Background(),
		`SELECT display_name FROM identity_connections WHERE id = $1`, connID).Scan(&got); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if got != newName {
		t.Fatalf("rename did not apply: got %q", got)
	}
}

// Test connection must report failure, never a success that implies the
// credentials work.
func TestTestIdpConnection_ReportsNotOkOnRejectedCredentials(t *testing.T) {
	h := newScimEnableHarness(t)
	defer h.teardown()
	mr := h.mutationResolver()

	issuer := startRejectingDiscoveryFixture(t)
	ws, connID := h.seedConnection(issuer)

	res, err := mr.TestIdpConnection(h.ctxFor(ws), connID)
	if err != nil {
		t.Fatalf("TestIdpConnection should report through the result, not error: %v", err)
	}
	if res.Ok {
		t.Fatal("Ok=true for credentials the IdP rejected")
	}
	if res.ScimEnabledAllowed {
		t.Fatal("ScimEnabledAllowed must stay false when the credentials are unverified")
	}
	if res.Reason == nil {
		t.Fatal("expected a reason explaining the failure")
	}
	if !strings.Contains(*res.Reason, "invalid_client") {
		t.Fatalf("the reason should name the IdP's error code, got %q", *res.Reason)
	}
}
