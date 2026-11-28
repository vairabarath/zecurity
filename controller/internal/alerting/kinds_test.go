package alerting

import (
	"errors"
	"testing"
)

func TestParseParams(t *testing.T) {
	for _, c := range []struct {
		kind, raw string
		ok        bool
		check     func(Params) bool
	}{
		{KindRelayOverCapacity, ``, true, func(p Params) bool { return p.ThresholdPercent == DefaultThresholdPercent }},
		{KindRelayOverCapacity, `{"threshold_percent": 75}`, true, func(p Params) bool { return p.ThresholdPercent == 75 }},
		{KindRelayOverCapacity, `{"threshold_percent": 0}`, true, func(p Params) bool { return p.ThresholdPercent == DefaultThresholdPercent }},
		{KindRelayOverCapacity, `{"threshold_percent": 1001}`, false, nil},
		{KindRelayOverCapacity, `{"threshold_percent": -5}`, false, nil},
		{KindRelayOverCapacity, `{"threshold_percent": "90"}`, false, nil},
		{KindRelayOverCapacity, `{"days": 3}`, false, nil},
		{KindCertificateExpiring, `{}`, true, func(p Params) bool { return p.Days == DefaultCertDays }},
		{KindCertificateExpiring, `{"days": 366}`, false, nil},
		{KindControlPlane, `{"components": ["database", "valkey"]}`, true, func(p Params) bool { return len(p.Components) == 2 }},
		{KindControlPlane, `{"components": [""]}`, false, nil},
		{KindTenantOverQuota, `{"keys": ["users"]}`, true, func(p Params) bool { return len(p.Keys) == 1 }},
		{KindTenantOverQuota, `{"keys": ["sessions"]}`, false, nil},
		{KindRelayOffline, `{}`, true, nil},
		{KindRelayOffline, `null`, true, nil},
		{KindRelayOffline, `{"threshold_percent": 5}`, false, nil},
		{KindConnectorOffline, `{"bogus": 1}`, false, nil},
		{KindConnectorOffline, `[]`, false, nil},
		{KindConnectorOffline, `{} {}`, false, nil},
		// tenant.usage_over (Sprint 25 U4, D-61): meter and threshold, both required.
		{KindTenantUsageOver, `{"meter": "relay_bytes_to_clients", "threshold": 500000000000}`, true, func(p Params) bool { return p.Meter == "relay_bytes_to_clients" && p.Threshold == 500_000_000_000 }},
		{KindTenantUsageOver, `{"meter": "active_users", "threshold": 50}`, true, nil},
		{KindTenantUsageOver, `{"meter": "bytes", "threshold": 5}`, false, nil},
		{KindTenantUsageOver, `{"meter": "session_minutes"}`, false, nil},
		{KindTenantUsageOver, `{"threshold": 5}`, false, nil},
		{KindTenantUsageOver, `{"meter": "session_minutes", "threshold": -1}`, false, nil},
		{KindTenantUsageOver, `{"meter": "session_minutes", "threshold": 1000000000000000001}`, false, nil},
		{KindTenantUsageOver, `{"meter": "session_minutes", "threshold": 5, "days": 3}`, false, nil},
		{KindTenantOverQuota, `{"meter": "session_minutes"}`, false, nil},
		{"nope", `{}`, false, nil},
	} {
		p, err := ParseParams(c.kind, []byte(c.raw))
		if c.ok != (err == nil) {
			t.Errorf("%s %s: ok=%v, err=%v", c.kind, c.raw, c.ok, err)
			continue
		}
		if !c.ok && !errors.Is(err, ErrInvalidParams) {
			t.Errorf("%s %s: want ErrInvalidParams, got %v", c.kind, c.raw, err)
		}
		if c.ok && c.check != nil && !c.check(p) {
			t.Errorf("%s %s: unexpected params %+v", c.kind, c.raw, p)
		}
	}
}

// Every kind has exactly one scope; relay-ops see relay and pool kinds only.
func TestKindScopes(t *testing.T) {
	platform, tenant := 0, 0
	for _, k := range Kinds() {
		switch ScopeOf(k) {
		case ScopePlatform:
			platform++
		case ScopeTenant:
			tenant++
			if relayOpsVisible(k) {
				t.Errorf("relay-ops must never see tenant kind %s", k)
			}
		default:
			t.Errorf("kind %s has no scope", k)
		}
	}
	// Sprint 27 G1 (077_): provider.break_glass_used and api_key.expiring.
	if platform != 7 || tenant != 5 {
		t.Fatalf("platform=%d tenant=%d, want 7 and 5", platform, tenant)
	}
	for _, k := range RelayOpsKinds() {
		if ScopeOf(k) != ScopePlatform {
			t.Errorf("relay-ops kind %s isn't platform", k)
		}
	}
}

func TestRuleValidate(t *testing.T) {
	ok := Rule{Kind: KindRelayOffline, Scope: ScopePlatform, Name: "x", Severity: SeverityWarning}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*Rule){
		"scope mismatch":   func(r *Rule) { r.Scope = ScopeTenant },
		"empty name":       func(r *Rule) { r.Name = "" },
		"padded name":      func(r *Rule) { r.Name = " x" },
		"control char":     func(r *Rule) { r.Name = "a\nb" },
		"bad severity":     func(r *Rule) { r.Severity = "high" },
		"for too long":     func(r *Rule) { r.ForSeconds = MaxForSeconds + 1 },
		"negative for":     func(r *Rule) { r.ForSeconds = -1 },
		"renotify too low": func(r *Rule) { r.RenotifySeconds = 60 },
	} {
		r := ok
		mut(&r)
		if err := r.Validate(); !errors.Is(err, ErrInvalidRule) {
			t.Errorf("%s: want ErrInvalidRule, got %v", name, err)
		}
	}
}

// Sprint 27 G1 / K1: the two new platform kinds and their parameters.
func TestNewProviderKinds(t *testing.T) {
	for _, k := range []string{KindBreakGlassUsed, KindAPIKeyExpiring} {
		if ScopeOf(k) != ScopePlatform || relayOpsVisible(k) {
			t.Errorf("%s: scope %q, relay-ops visible %v", k, ScopeOf(k), relayOpsVisible(k))
		}
	}
	if p, err := ParseParams(KindBreakGlassUsed, []byte(`{}`)); err != nil || p.DaysBefore != 0 {
		t.Fatalf("break-glass {}: %+v %v", p, err)
	}
	if _, err := ParseParams(KindBreakGlassUsed, []byte(`{"days_before": 3}`)); err == nil {
		t.Fatal("break-glass took days_before")
	}
	if p, err := ParseParams(KindAPIKeyExpiring, []byte(`{}`)); err != nil || p.DaysBefore != DefaultAPIKeyDaysBefore {
		t.Fatalf("api_key.expiring default: %+v %v", p, err)
	}
	if p, err := ParseParams(KindAPIKeyExpiring, []byte(`{"days_before": 30}`)); err != nil || p.DaysBefore != 30 {
		t.Fatalf("api_key.expiring 30: %+v %v", p, err)
	}
	for _, raw := range []string{`{"days_before": 91}`, `{"days_before": -1}`, `{"days": 14}`, `{"threshold_percent": 5}`} {
		if _, err := ParseParams(KindAPIKeyExpiring, []byte(raw)); err == nil {
			t.Errorf("api_key.expiring accepted %s", raw)
		}
	}
	if _, err := ParseParams(KindCertificateExpiring, []byte(`{"days_before": 14}`)); err == nil {
		t.Error("certificate.expiring took days_before")
	}
}
