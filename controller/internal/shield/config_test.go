package shield

import (
	"testing"
	"time"
)

// Sprint 21 K2: the window sent to connectors is min(window, 2/5 × TTL).
func TestEffectiveRenewalWindow(t *testing.T) {
	cases := []struct {
		name          string
		window, ttl   time.Duration
		want          time.Duration
		freshInWindow bool // would a just-issued cert (expires in ttl) be inside it?
	}{
		{"defaults: 48h window, 7d TTL", 48 * time.Hour, 7 * 24 * time.Hour, 48 * time.Hour, false},
		{"TTL 48h clamps the 48h window", 48 * time.Hour, 48 * time.Hour, 48 * time.Hour * 2 / 5, false},
		{"TTL 24h", 48 * time.Hour, 24 * time.Hour, 24 * time.Hour * 2 / 5, false},
		{"TTL 1h → 24m", 48 * time.Hour, time.Hour, 24 * time.Minute, false},
		{"TTL 30m → 12m", 48 * time.Hour, 30 * time.Minute, 12 * time.Minute, false},
		{"window smaller than 2/5 TTL is kept", time.Hour, 7 * 24 * time.Hour, time.Hour, false},
		{"zero window", 0, 7 * 24 * time.Hour, 0, false},
		{"zero TTL", 48 * time.Hour, 0, 0, false},
	}
	for _, tc := range cases {
		got := Config{RenewalWindow: tc.window, CertTTL: tc.ttl}.EffectiveRenewalWindow()
		if got != tc.want {
			t.Errorf("%s: EffectiveRenewalWindow = %s, want %s", tc.name, got, tc.want)
		}
		// The point of the clamp: a fresh certificate is never inside it.
		if tc.ttl > 0 && got > 0 && (tc.ttl <= got) != tc.freshInWindow {
			t.Errorf("%s: fresh cert (expires in %s) inside window %s", tc.name, tc.ttl, got)
		}
	}
}
