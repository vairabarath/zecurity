package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yourorg/ztna/controller/internal/idp"
)

func discoveryStore() *fakeConnStore {
	tenant := "ws-acme-id"
	return &fakeConnStore{
		workspaces: map[string]string{"acme": tenant, "empty": "ws-empty-id"},
		connections: []idp.Connection{
			{ID: "google-conn", Provider: "google", Managed: true, Protocol: "oidc", DisplayName: "Google", Status: "active"}, // TenantID nil = bootstrap
			{ID: "okta-conn", Provider: "oidc", Protocol: "oidc", DisplayName: "Acme Okta", Status: "active", TenantID: &tenant},
			{ID: "old-conn", Provider: "oidc", Protocol: "oidc", DisplayName: "Old", Status: "disabled", TenantID: &tenant},
		},
	}
}

func doDiscovery(t *testing.T, svc *serviceImpl, slug string) (*httptest.ResponseRecorder, discoveryResponse) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/workspaces/"+slug+"/auth", nil)
	req.SetPathValue("slug", slug)
	w := httptest.NewRecorder()
	svc.DiscoveryHandler().ServeHTTP(w, req)
	var resp discoveryResponse
	if w.Code == http.StatusOK {
		_ = json.NewDecoder(w.Body).Decode(&resp)
	}
	return w, resp
}

func TestDiscovery_ConfiguredWorkspace_EnterpriseFirst(t *testing.T) {
	svc := &serviceImpl{idpStore: discoveryStore()}
	w, resp := doDiscovery(t, svc, "acme")
	if w.Code != http.StatusOK {
		t.Fatalf("code %d", w.Code)
	}
	if resp.Workspace != "acme" {
		t.Fatalf("workspace %q", resp.Workspace)
	}
	// disabled connection excluded; enterprise first, bootstrap fallback.
	if len(resp.Providers) != 2 {
		t.Fatalf("want 2 active providers, got %+v", resp.Providers)
	}
	if resp.Providers[0].Tier != "enterprise" || resp.Providers[0].ID != "okta-conn" {
		t.Fatalf("enterprise must be first: %+v", resp.Providers)
	}
	if resp.Providers[1].Tier != "bootstrap" || resp.Providers[1].ID != "google-conn" {
		t.Fatalf("bootstrap must follow: %+v", resp.Providers)
	}
	if !resp.PlatformFallback {
		t.Fatal("platformFallback should be true")
	}
}

func TestDiscovery_UnconfiguredWorkspace_BootstrapOnly(t *testing.T) {
	svc := &serviceImpl{idpStore: discoveryStore()}
	w, resp := doDiscovery(t, svc, "empty")
	if w.Code != http.StatusOK {
		t.Fatalf("code %d", w.Code)
	}
	if len(resp.Providers) != 1 || resp.Providers[0].Tier != "bootstrap" || resp.Providers[0].ID != "google-conn" {
		t.Fatalf("want only bootstrap Google, got %+v", resp.Providers)
	}
	if !resp.PlatformFallback {
		t.Fatal("platformFallback should be true")
	}
}

// TestDiscovery_MultipleEnterprise_DeterministicOrder locks the ordering when a
// workspace has several Enterprise IdPs: enterprise first (alphabetical by
// display, mirroring the store's ORDER BY), then Bootstrap fallback.
func TestDiscovery_MultipleEnterprise_DeterministicOrder(t *testing.T) {
	tenant := "ws-acme-id"
	store := &fakeConnStore{
		workspaces: map[string]string{"acme": tenant},
		connections: []idp.Connection{
			// intentionally inserted out of order to prove ordering isn't incidental
			{ID: "okta-conn", Provider: "oidc", Protocol: "oidc", DisplayName: "Acme Okta", Status: "active", TenantID: &tenant},
			{ID: "google-conn", Provider: "google", Managed: true, Protocol: "oidc", DisplayName: "Google", Status: "active"},
			{ID: "entra-conn", Provider: "oidc", Protocol: "oidc", DisplayName: "Acme Entra", Status: "active", TenantID: &tenant},
		},
	}
	svc := &serviceImpl{idpStore: store}
	w, resp := doDiscovery(t, svc, "acme")
	if w.Code != http.StatusOK {
		t.Fatalf("code %d", w.Code)
	}
	gotIDs := []string{resp.Providers[0].ID, resp.Providers[1].ID, resp.Providers[2].ID}
	gotTiers := []string{resp.Providers[0].Tier, resp.Providers[1].Tier, resp.Providers[2].Tier}
	// Enterprise first, alphabetical by display ("Acme Entra" < "Acme Okta"); Google (bootstrap) last.
	wantIDs := []string{"entra-conn", "okta-conn", "google-conn"}
	wantTiers := []string{"enterprise", "enterprise", "bootstrap"}
	for i := range wantIDs {
		if gotIDs[i] != wantIDs[i] || gotTiers[i] != wantTiers[i] {
			t.Fatalf("order mismatch at %d: got (%s,%s) want (%s,%s); full=%+v", i, gotIDs[i], gotTiers[i], wantIDs[i], wantTiers[i], resp.Providers)
		}
	}
	if !resp.PlatformFallback {
		t.Fatal("platformFallback should be true")
	}
}

// TestDiscovery_PlatformLoginDisabled_OmitsBootstrap covers the Phase 7 toggle:
// when a workspace disables platform login, the Bootstrap tier is not advertised
// and platformFallback is false — only the workspace's own Enterprise IdP(s).
func TestDiscovery_PlatformLoginDisabled_OmitsBootstrap(t *testing.T) {
	store := discoveryStore()
	store.platformDisabled = map[string]bool{"ws-acme-id": true}
	svc := &serviceImpl{idpStore: store}

	w, resp := doDiscovery(t, svc, "acme")
	if w.Code != http.StatusOK {
		t.Fatalf("code %d", w.Code)
	}
	if len(resp.Providers) != 1 || resp.Providers[0].Tier != "enterprise" || resp.Providers[0].ID != "okta-conn" {
		t.Fatalf("want only the enterprise Okta IdP, got %+v", resp.Providers)
	}
	if resp.PlatformFallback {
		t.Fatal("platformFallback must be false when platform login is disabled")
	}
}

func TestDiscovery_UnknownWorkspace_404(t *testing.T) {
	svc := &serviceImpl{idpStore: discoveryStore()}
	w, _ := doDiscovery(t, svc, "nope")
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", w.Code)
	}
}

// A response must never carry a client secret (invariant doc.go #8). Enterprise
// connections would have ClientSecret populated after decrypt; discovery must
// only surface id/display/type/tier.
func TestDiscovery_NeverLeaksSecret(t *testing.T) {
	tenant := "ws-acme-id"
	store := &fakeConnStore{
		workspaces: map[string]string{"acme": tenant},
		connections: []idp.Connection{
			{ID: "okta-conn", Provider: "oidc", Protocol: "oidc", DisplayName: "Acme Okta", Status: "active", TenantID: &tenant, ClientSecret: "TOP-SECRET"},
		},
	}
	svc := &serviceImpl{idpStore: store}
	req := httptest.NewRequest(http.MethodGet, "/workspaces/acme/auth", nil)
	req.SetPathValue("slug", "acme")
	w := httptest.NewRecorder()
	svc.DiscoveryHandler().ServeHTTP(w, req)
	if got := w.Body.String(); strings.Contains(got, "TOP-SECRET") || strings.Contains(got, "client_secret") {
		t.Fatalf("discovery response leaked secret material: %s", got)
	}
}
