package provider

import (
	"testing"
	"time"
)

func TestProviderTokenRoundTrip(t *testing.T) {
	secret := "test-secret"
	tok, err := IssueProviderToken(secret, "uid-1", RoleSuperAdmin, "a@corp.com", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := VerifyProviderToken(secret, tok)
	if err != nil {
		t.Fatal(err)
	}
	if claims.Subject != "uid-1" || claims.Role != RoleSuperAdmin || claims.Email != "a@corp.com" {
		t.Fatalf("claims mismatch: %+v", claims)
	}
}

func TestVerifyRejectsWrongSecret(t *testing.T) {
	tok, _ := IssueProviderToken("secret-a", "uid", RoleRelayOps, "b@corp.com", time.Minute)
	if _, err := VerifyProviderToken("secret-b", tok); err == nil {
		t.Fatal("expected verification to fail with wrong secret")
	}
}
