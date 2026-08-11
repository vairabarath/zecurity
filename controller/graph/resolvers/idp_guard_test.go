package resolvers

import "testing"

// The no-lockout guard (ADR-024 §5): removing a workspace's last active login
// path is refused ONLY when there is no platform fallback and the actor is not a
// break-glass admin.
func TestLastLoginPathGuard(t *testing.T) {
	cases := []struct {
		name        string
		activeConns int
		platformOK  bool
		breakGlass  bool
		wantErr     bool
	}{
		{"more than one connection is always fine", 2, false, false, false},
		{"platform fallback keeps it safe", 1, true, false, false},
		{"break-glass admin keeps it safe", 1, false, true, false},
		{"platform + break-glass both fine", 1, true, true, false},
		{"last path, no fallback, not break-glass → refused", 1, false, false, true},
		{"zero connections, no fallback, not break-glass → refused", 0, false, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := lastLoginPathGuard(c.activeConns, c.platformOK, c.breakGlass)
			if c.wantErr && err == nil {
				t.Fatalf("expected refusal, got nil")
			}
			if !c.wantErr && err != nil {
				t.Fatalf("expected nil, got %v", err)
			}
		})
	}
}

// Disabling platform login (Phase 7) is refused when the workspace has no active
// IdP of its own, unless the actor is a break-glass admin.
func TestPlatformDisableGuard(t *testing.T) {
	cases := []struct {
		name       string
		activeOwn  int
		breakGlass bool
		wantErr    bool
	}{
		{"has own IdP → safe to disable platform", 1, false, false},
		{"multiple own IdPs → safe", 3, false, false},
		{"no own IdP but break-glass admin → allowed", 0, true, false},
		{"no own IdP, not break-glass → refused", 0, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := platformDisableGuard(c.activeOwn, c.breakGlass)
			if c.wantErr && err == nil {
				t.Fatalf("expected refusal, got nil")
			}
			if !c.wantErr && err != nil {
				t.Fatalf("expected nil, got %v", err)
			}
		})
	}
}
