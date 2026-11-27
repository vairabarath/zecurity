// Package alerting is F26 alert rules (Sprint 24 A1, Decision Record D-50,
// D-53): built-in rule kinds with parameters, an evaluator that turns their
// conditions into pending → firing → resolved states, and the append-only
// event history. Delivery (webhook, email) is A2; it receives the events
// through Notifier.
//
// Scopes (D-50): every kind is either a platform kind (the provider's
// infrastructure; no tenant) or a tenant kind (one tenant can act on it;
// always a tenant_id). The database enforces the split (069_alerts.sql);
// this package never builds a row that would break it.
//
// Single controller (D-02): the evaluator is one process-local loop.
package alerting

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/yourorg/ztna/controller/internal/providerquery"
	"github.com/yourorg/ztna/controller/internal/quota"
)

// Scopes (D-50).
const (
	ScopePlatform = "platform"
	ScopeTenant   = "tenant"
)

// Severities, most severe first.
const (
	SeverityCritical = "critical"
	SeverityWarning  = "warning"
	SeverityInfo     = "info"
)

// Rule kinds. Each belongs to exactly one scope (kindScope, and the
// alert_rules_kind_scope CHECK in 069_alerts.sql).
const (
	KindRelayOffline        = "relay.offline"
	KindRelayOverCapacity   = "relay.over_capacity"
	KindPoolNoEligibleRelay = "pool.no_eligible_relay"
	KindControlPlane        = "controlplane.degraded"
	KindCertificateExpiring = "certificate.expiring"
	KindConnectorOffline    = "connector.offline"
	KindShieldOffline       = "shield.offline"
	KindTenantNoRelayPath   = "tenant.no_relay_path"
	KindTenantOverQuota     = "tenant.over_quota"
	// KindTenantUsageOver (Sprint 25 U4, D-61): a tenant's month-to-date
	// usage of one meter at or above a threshold. Warns only.
	KindTenantUsageOver = "tenant.usage_over"
	// KindBreakGlassUsed (Sprint 27 G1, D-75): recover-admin was run on the
	// controller host. One alert per unacknowledged break-glass event,
	// resolved when a super-admin acknowledges it.
	KindBreakGlassUsed = "provider.break_glass_used"
	// KindAPIKeyExpiring (Sprint 27 K1, D-76 Q6): an active provider API key
	// expires within days_before days (default 14). Fires once per key;
	// resolves on revoke, expiry or leaving the window.
	KindAPIKeyExpiring = "api_key.expiring"
)

var kindScope = map[string]string{
	KindRelayOffline:        ScopePlatform,
	KindRelayOverCapacity:   ScopePlatform,
	KindPoolNoEligibleRelay: ScopePlatform,
	KindControlPlane:        ScopePlatform,
	KindCertificateExpiring: ScopePlatform,
	KindConnectorOffline:    ScopeTenant,
	KindShieldOffline:       ScopeTenant,
	KindTenantNoRelayPath:   ScopeTenant,
	KindTenantOverQuota:     ScopeTenant,
	KindTenantUsageOver:     ScopeTenant,
	KindBreakGlassUsed:      ScopePlatform,
	KindAPIKeyExpiring:      ScopePlatform,
}

// Kinds returns every kind, sorted.
func Kinds() []string {
	out := make([]string, 0, len(kindScope))
	for k := range kindScope {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ScopeOf returns the kind's scope, or "" for an unknown kind.
func ScopeOf(kind string) string { return kindScope[kind] }

// relayOpsVisible reports whether relay-ops may see alerts of this kind
// (D-40 extended, D-50): platform relay and pool alerts only, never tenant
// alerts or the control plane / certificate alerts.
func relayOpsVisible(kind string) bool {
	return kind == KindRelayOffline || kind == KindRelayOverCapacity || kind == KindPoolNoEligibleRelay
}

// ValidSeverity reports whether s is a known severity.
func ValidSeverity(s string) bool {
	return s == SeverityCritical || s == SeverityWarning || s == SeverityInfo
}

// Limits on rule timing (mirror the CHECKs in 069_alerts.sql).
const (
	MaxForSeconds      = 86400
	MinRenotifySeconds = 300
	MaxRenotifySeconds = 604800
)

// ErrInvalidParams: the parameters don't fit the kind.
var ErrInvalidParams = errors.New("alerting: invalid params")

// Params are a rule's kind-specific parameters, already validated.
type Params struct {
	// relay.over_capacity: fire at or above this share of link capacity.
	ThresholdPercent int `json:"threshold_percent,omitempty"`
	// certificate.expiring: CA certificates within this many days.
	Days int `json:"days,omitempty"`
	// controlplane.degraded: only these components (empty = every component).
	Components []string `json:"components,omitempty"`
	// tenant.over_quota: only these quota keys (empty = every key).
	Keys []string `json:"keys,omitempty"`
	// tenant.usage_over (D-61): the usage meter (D-57) and its monthly
	// threshold in the meter's unit (bytes, minutes or users). Both required.
	Meter     string `json:"meter,omitempty"`
	Threshold int64  `json:"threshold,omitempty"`
	// api_key.expiring (D-76 Q6): fire this many days before a key expires.
	DaysBefore int `json:"days_before,omitempty"`
}

// MaxUsageThreshold bounds tenant.usage_over's threshold (an exabyte).
const MaxUsageThreshold = int64(1_000_000_000_000_000_000)

// Defaults applied when a parameter is left out.
const (
	DefaultThresholdPercent = 90
	DefaultCertDays         = 30
	DefaultAPIKeyDaysBefore = 14
)

// ParseParams validates raw parameters for kind and returns them with
// defaults applied. Unknown fields, wrong types and out-of-range values are
// ErrInvalidParams; so is a parameter the kind doesn't take.
func ParseParams(kind string, raw json.RawMessage) (Params, error) {
	var p Params
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) != 0 && !bytes.Equal(trimmed, []byte("null")) {
		dec := json.NewDecoder(bytes.NewReader(trimmed))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&p); err != nil {
			return Params{}, fmt.Errorf("%w: %v", ErrInvalidParams, err)
		}
		if dec.More() {
			return Params{}, fmt.Errorf("%w: trailing data", ErrInvalidParams)
		}
	}
	only := func(allowed ...string) error {
		set := map[string]bool{
			"threshold_percent": p.ThresholdPercent != 0,
			"days":              p.Days != 0,
			"components":        p.Components != nil,
			"keys":              p.Keys != nil,
			"meter":             p.Meter != "",
			"threshold":         p.Threshold != 0,
			"days_before":       p.DaysBefore != 0,
		}
		ok := map[string]bool{}
		for _, a := range allowed {
			ok[a] = true
		}
		for name, given := range set {
			if given && !ok[name] {
				return fmt.Errorf("%w: %s doesn't take %s", ErrInvalidParams, kind, name)
			}
		}
		return nil
	}
	switch kind {
	case KindRelayOverCapacity:
		if err := only("threshold_percent"); err != nil {
			return Params{}, err
		}
		if p.ThresholdPercent == 0 {
			p.ThresholdPercent = DefaultThresholdPercent
		}
		if p.ThresholdPercent < 1 || p.ThresholdPercent > 1000 {
			return Params{}, fmt.Errorf("%w: threshold_percent must be 1..1000", ErrInvalidParams)
		}
	case KindCertificateExpiring:
		if err := only("days"); err != nil {
			return Params{}, err
		}
		if p.Days == 0 {
			p.Days = DefaultCertDays
		}
		if p.Days < 1 || p.Days > 365 {
			return Params{}, fmt.Errorf("%w: days must be 1..365", ErrInvalidParams)
		}
	case KindControlPlane:
		if err := only("components"); err != nil {
			return Params{}, err
		}
		if len(p.Components) > 20 {
			return Params{}, fmt.Errorf("%w: at most 20 components", ErrInvalidParams)
		}
		for _, c := range p.Components {
			if c == "" || len(c) > 64 {
				return Params{}, fmt.Errorf("%w: invalid component name", ErrInvalidParams)
			}
		}
	case KindTenantOverQuota:
		if err := only("keys"); err != nil {
			return Params{}, err
		}
		for _, k := range p.Keys {
			if !quota.ValidKey(k) {
				return Params{}, fmt.Errorf("%w: unknown quota key %q", ErrInvalidParams, k)
			}
		}
	case KindTenantUsageOver:
		if err := only("meter", "threshold"); err != nil {
			return Params{}, err
		}
		if !providerquery.IsUsageMeter(p.Meter) {
			return Params{}, fmt.Errorf("%w: meter must be one of the usage meters", ErrInvalidParams)
		}
		if p.Threshold < 1 || p.Threshold > MaxUsageThreshold {
			return Params{}, fmt.Errorf("%w: threshold must be 1..%d", ErrInvalidParams, MaxUsageThreshold)
		}
	case KindAPIKeyExpiring:
		if err := only("days_before"); err != nil {
			return Params{}, err
		}
		if p.DaysBefore == 0 {
			p.DaysBefore = DefaultAPIKeyDaysBefore
		}
		if p.DaysBefore < 1 || p.DaysBefore > 90 {
			return Params{}, fmt.Errorf("%w: days_before must be 1..90", ErrInvalidParams)
		}
	case KindRelayOffline, KindPoolNoEligibleRelay, KindConnectorOffline, KindShieldOffline, KindTenantNoRelayPath,
		KindBreakGlassUsed:
		if err := only(); err != nil {
			return Params{}, err
		}
	default:
		return Params{}, fmt.Errorf("%w: unknown kind %q", ErrInvalidParams, kind)
	}
	return p, nil
}

// JSON returns the parameters as stored (defaults included).
func (p Params) JSON() []byte {
	b, _ := json.Marshal(p)
	return b
}
