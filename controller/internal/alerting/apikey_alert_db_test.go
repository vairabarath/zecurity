package alerting

import (
	"context"
	"encoding/hex"
	"strings"
	"testing"
	"time"
)

// Sprint 27 K1 part 2 (D-76 Q6): api_key.expiring fires once per active key
// 14 days before it expires (test clock), never for a revoked or expired key,
// resolves on revoke, on expiry and when the window is lowered, and carries
// no secret.
const ruleAPIKeyExpiring = "00000000-0000-0000-0002-000000000012"

func TestAPIKeyExpiringDB(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if n := e.count(t, `SELECT count(*) FROM alert_rules WHERE id = $1 AND kind = 'api_key.expiring' AND scope = 'platform'
	                      AND enabled AND severity = 'warning' AND params = '{"days_before": 14}'::jsonb`, ruleAPIKeyExpiring); n != 1 {
		t.Fatalf("seeded api_key.expiring rule missing or changed")
	}
	e.onlyRule(t, ruleAPIKeyExpiring)

	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	hash := strings.Repeat("ab", 32)
	key := func(name, prefix string, created time.Time, expiresIn time.Duration) string {
		return e.one(t, `INSERT INTO provider_api_keys (name, role, prefix, secret_sha256, created_by, created_at, expires_at)
		                 VALUES ($1, 'auditor', $2, decode($3, 'hex'), $4, $5, $6) RETURNING id::text`,
			name, prefix, hash, e.superID, created, now.Add(expiresIn))
	}
	day := 24 * time.Hour
	soon := key("ci-reader", "zpk_aaaaaaaa", now.Add(-300*day), 10*day)        // inside 14 days
	later := key("nightly", "zpk_bbbbbbbb", now.Add(-10*day), 20*day)          // outside
	edge := key("edge", "zpk_cccccccc", now.Add(-100*day), 14*day)             // exactly 14 days: inside
	revoked := key("old-script", "zpk_dddddddd", now.Add(-300*day), 5*day)     // revoked below: never fires
	expired := key("expired", "zpk_eeeeeeee", now.Add(-365*day), -1*time.Hour) // already expired: never fires
	_ = expired
	e.exec(t, `UPDATE provider_api_keys SET revoked_at = $2, revoked_by = $3, revoke_reason = 'rotated' WHERE id = $1`, revoked, now.Add(-day), e.superID)

	clock := now
	ev := &Evaluator{Store: e.store, Observer: Sources{DB: e.pool}, Now: func() time.Time { return clock }}
	fired := ev.Tick(ctx)
	got := map[string]Event{}
	for _, f := range fired {
		got[f.TargetID] = f
	}
	if len(fired) != 2 || got[soon].Transition != TransitionFired || got[edge].Transition != TransitionFired {
		t.Fatalf("fired %d: %+v", len(fired), fired)
	}
	for _, f := range fired {
		if f.Kind != KindAPIKeyExpiring || f.Scope != ScopePlatform || f.TenantID != nil || f.Severity != SeverityWarning || f.TargetType != "api_key" {
			t.Fatalf("event: %+v", f)
		}
		payload := string(NewPayload(f, "https://console.test").JSON())
		if strings.Contains(payload, hash) || strings.Contains(payload, hex.EncodeToString([]byte(hash))) ||
			!strings.Contains(payload, "role auditor") || !strings.Contains(payload, "zpk_") {
			t.Fatalf("payload (names key, prefix, role; never the hash): %s", payload)
		}
	}
	if !strings.Contains(got[soon].Summary, "ci-reader (zpk_aaaaaaaa, role auditor) expires at 2026-12-07T12:00:00Z") {
		t.Fatalf("summary: %q", got[soon].Summary)
	}

	// Once: later ticks don't re-fire.
	clock = now.Add(time.Hour)
	if again := ev.Tick(ctx); len(again) != 0 {
		t.Fatalf("re-fired: %+v", again)
	}

	// Revoking the key resolves its alert.
	e.exec(t, `UPDATE provider_api_keys SET revoked_at = $2, revoked_by = $3, revoke_reason = 'replaced' WHERE id = $1`, soon, clock, e.superID)
	res := ev.Tick(ctx)
	if len(res) != 1 || res[0].TargetID != soon || res[0].Transition != TransitionResolved {
		t.Fatalf("resolve on revoke: %+v", res)
	}

	// Six days later "later" (20 days out at the start) enters the window.
	clock = now.Add(6*day + time.Hour)
	if f := ev.Tick(ctx); len(f) != 1 || f[0].TargetID != later || f[0].Transition != TransitionFired {
		t.Fatalf("later entering the window: %+v", f)
	}

	// After "edge" expires its alert resolves (an expired key no longer
	// authenticates and needs no warning).
	clock = now.Add(14*day + time.Minute)
	res = ev.Tick(ctx)
	if len(res) != 1 || res[0].TargetID != edge || res[0].Transition != TransitionResolved {
		t.Fatalf("resolve on expiry: %+v", res)
	}

	// Lowering the window to 1 day resolves "later" (5 days left).
	e.exec(t, `UPDATE alert_rules SET params = '{"days_before": 1}' WHERE id = $1`, ruleAPIKeyExpiring)
	res = ev.Tick(ctx)
	if len(res) != 1 || res[0].TargetID != later || res[0].Transition != TransitionResolved {
		t.Fatalf("resolve when out of the window: %+v", res)
	}

	// The revoked and the expired key never fired.
	if n := e.count(t, `SELECT count(*) FROM alert_events WHERE target_id IN ($1, $2)`, revoked, expired); n != 0 {
		t.Fatalf("events for a revoked or expired key: %d", n)
	}
}
