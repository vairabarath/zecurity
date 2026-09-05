package client

import (
	"testing"

	"github.com/yourorg/ztna/controller/internal/idp"
)

func TestSelectEffectiveConnection(t *testing.T) {
	tenant := "ws-1"
	google := idp.Connection{ID: "google", Provider: "google", Managed: true, Status: "active"} // TenantID nil = bootstrap
	okta := idp.Connection{ID: "okta", Provider: "oidc", Status: "active", TenantID: &tenant}
	entra := idp.Connection{ID: "entra", Provider: "oidc", Status: "active", TenantID: &tenant}
	disabledEnt := idp.Connection{ID: "old", Provider: "oidc", Status: "disabled", TenantID: &tenant}

	t.Run("exactly one enterprise -> use it (disabled ignored)", func(t *testing.T) {
		conn, err := selectEffectiveConnection([]idp.Connection{google, okta, disabledEnt})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if conn.ID != "okta" {
			t.Fatalf("want okta, got %s", conn.ID)
		}
	})

	t.Run("zero enterprise -> bootstrap fallback", func(t *testing.T) {
		conn, err := selectEffectiveConnection([]idp.Connection{google, disabledEnt})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if conn.ID != "google" {
			t.Fatalf("want bootstrap google, got %s", conn.ID)
		}
	})

	t.Run("multiple enterprise -> explicit error", func(t *testing.T) {
		if _, err := selectEffectiveConnection([]idp.Connection{google, okta, entra}); err == nil {
			t.Fatal("expected error for multiple enterprise IdPs (CLI cannot disambiguate)")
		}
	})

	t.Run("no active provider -> error", func(t *testing.T) {
		if _, err := selectEffectiveConnection([]idp.Connection{disabledEnt}); err == nil {
			t.Fatal("expected error when no active provider is configured")
		}
	})
}
