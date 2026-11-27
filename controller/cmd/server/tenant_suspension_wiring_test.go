package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// Sprint 23 S1 (D-45): every tenant console route that has WorkspaceGuard
// also has TenantEligibilityGuard. main.go wraps both in one helper
// (workspaceGuard); a route that called middleware.WorkspaceGuard directly
// would skip the suspension check. The provider tenant routes are wired too.
func TestTenantSuspensionWiring(t *testing.T) {
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	direct := regexp.MustCompile(`middleware\.WorkspaceGuard\(`).FindAllStringIndex(src, -1)
	if len(direct) != 1 {
		t.Fatalf("middleware.WorkspaceGuard is called %d times in main.go, want exactly 1 (inside workspaceGuard)", len(direct))
	}
	if !strings.Contains(src, "return middleware.WorkspaceGuard(db.Pool)(middleware.TenantEligibilityGuard(db.Pool)(next))") {
		t.Fatal("workspaceGuard no longer adds TenantEligibilityGuard")
	}
	if n := strings.Count(src, "workspaceGuard("); n < 8 {
		t.Fatalf("workspaceGuard used on %d routes, want every tenant console route (≥ 8)", n)
	}
	// Sprint 27 K1: requireProviderOrKey is RequireProvider that also
	// accepts a read-only API key (decide() refuses keys every write).
	for _, route := range []string{
		`"POST /provider/tenants/{id}/suspend", requireProviderOrKey(`,
		`"POST /provider/tenants/{id}/resume", requireProviderOrKey(`,
	} {
		if !strings.Contains(src, route) {
			t.Fatalf("route %s not wired behind requireProvider", route)
		}
	}
}
