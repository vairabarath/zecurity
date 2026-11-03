package main

import (
	"strings"
	"testing"
	"time"
)

func TestConnectorRenewalWarning(t *testing.T) {
	for _, tc := range []struct {
		name        string
		ttl, window time.Duration
		warn        bool
	}{
		{"defaults", 168 * time.Hour, 48 * time.Hour, false},
		{"dev short TTL", 15 * time.Minute, 10 * time.Minute, false},
		{"window equals TTL", 15 * time.Minute, 15 * time.Minute, true},
		{"window longer than TTL", 15 * time.Minute, 20 * time.Minute, true},
		{"unset window", 15 * time.Minute, 0, false},
	} {
		msg := connectorRenewalWarning(tc.ttl, tc.window)
		if (msg != "") != tc.warn {
			t.Errorf("%s: warning = %q, want warn=%v", tc.name, msg, tc.warn)
		}
		if tc.warn && !strings.Contains(msg, tc.ttl.String()) {
			t.Errorf("%s: warning does not name the TTL: %q", tc.name, msg)
		}
	}
}
