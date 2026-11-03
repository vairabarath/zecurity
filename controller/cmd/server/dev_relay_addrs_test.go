package main

import "testing"

// RELAY_DEV_ALLOW_PRIVATE_ADDR takes effect only together with ENV=development.
func TestDevAllowPrivateRelayAddrsRequiresDevelopment(t *testing.T) {
	for _, tc := range []struct {
		env, flag string
		want      bool
	}{
		{"development", "true", true},
		{"production", "true", false},
		{"", "true", false},
		{"development", "", false},
		{"development", "1", false},
	} {
		t.Setenv("ENV", tc.env)
		t.Setenv("RELAY_DEV_ALLOW_PRIVATE_ADDR", tc.flag)
		if got := devAllowPrivateRelayAddrs(); got != tc.want {
			t.Errorf("ENV=%q flag=%q: got %v, want %v", tc.env, tc.flag, got, tc.want)
		}
	}
}
