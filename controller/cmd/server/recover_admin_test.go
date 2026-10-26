package main

import (
	"bytes"
	"strings"
	"testing"
)

// Sprint 22 Phase T: `zecurity-controller provider recover-admin --email <e>`.

func TestIsRecoverAdminCommand(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{[]string{"zecurity-controller"}, false},
		{[]string{"zecurity-controller", "provider"}, false},
		{[]string{"zecurity-controller", "provider", "recover-admin", "--email", "a@b.c"}, true},
		{[]string{"zecurity-controller", "provider", "other"}, false},
		{[]string{"zecurity-controller", "recover-admin"}, false},
	} {
		if got := isRecoverAdminCommand(tc.args); got != tc.want {
			t.Errorf("%v: %v, want %v", tc.args, got, tc.want)
		}
	}
}

func TestParseRecoverAdminArgs(t *testing.T) {
	var stderr bytes.Buffer
	email, err := parseRecoverAdminArgs([]string{"--email", "Ops@InkYank.com"}, &stderr)
	if err != nil || email != "Ops@InkYank.com" {
		t.Fatalf("email=%q err=%v", email, err)
	}
	for _, args := range [][]string{nil, {"--email", ""}, {"--email", "   "}, {"--bogus"}, {"--email", "a@b.c", "extra"}} {
		if _, err := parseRecoverAdminArgs(args, &stderr); err == nil {
			t.Errorf("%v: accepted", args)
		}
	}
	if !strings.Contains(stderr.String(), "--email") {
		t.Errorf("usage does not mention --email: %q", stderr.String())
	}
}
