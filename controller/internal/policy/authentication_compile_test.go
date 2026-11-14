package policy

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/yourorg/ztna/controller/internal/posture"
)

// PENDING-06 M-4 / M-9: the pure decision. Fail closed on anything unknown.
func TestDeviceAuthenticated(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time { v := now.Add(d); return &v }
	hour := posture.AuthRequirement{Frequency: time.Hour}
	hourMFA := posture.AuthRequirement{Frequency: time.Hour, RequireMFA: true}

	cases := []struct {
		name      string
		req       posture.AuthRequirement
		d         DeviceIdentity
		want      bool
		wantUntil time.Time
	}{
		{"never authenticated", hour, DeviceIdentity{}, false, time.Time{}},
		{"fresh", hour, DeviceIdentity{AuthAt: at(-10 * time.Minute)}, true, now.Add(50 * time.Minute)},
		{"exactly expired", hour, DeviceIdentity{AuthAt: at(-time.Hour)}, false, time.Time{}},
		{"stale", hour, DeviceIdentity{AuthAt: at(-2 * time.Hour)}, false, time.Time{}},
		{"mfa required, none", hourMFA, DeviceIdentity{AuthAt: at(-time.Minute)}, false, time.Time{}},
		{"mfa required, stale mfa", hourMFA, DeviceIdentity{AuthAt: at(-time.Minute), MFAAt: at(-2 * time.Hour)}, false, time.Time{}},
		{"mfa required, both fresh: earliest expiry wins", hourMFA,
			DeviceIdentity{AuthAt: at(-10 * time.Minute), MFAAt: at(-30 * time.Minute)}, true, now.Add(30 * time.Minute)},
		{"mfa not required ignores a missing mfa", hour, DeviceIdentity{AuthAt: at(-time.Minute)}, true, now.Add(59 * time.Minute)},
		{"risk: authentication older than the demand", hour,
			DeviceIdentity{AuthAt: at(-10 * time.Minute), ReauthRequiredAt: at(-5 * time.Minute)}, false, time.Time{}},
		{"risk: authentication at the demand instant does not count", hour,
			DeviceIdentity{AuthAt: at(-5 * time.Minute), ReauthRequiredAt: at(-5 * time.Minute)}, false, time.Time{}},
		{"risk: authenticated after the demand", hour,
			DeviceIdentity{AuthAt: at(-time.Minute), ReauthRequiredAt: at(-5 * time.Minute)}, true, now.Add(59 * time.Minute)},
		{"risk: mfa older than the demand", hourMFA,
			DeviceIdentity{AuthAt: at(-time.Minute), MFAAt: at(-10 * time.Minute), ReauthRequiredAt: at(-5 * time.Minute)}, false, time.Time{}},
	}
	for _, c := range cases {
		got, until := DeviceAuthenticated(now, c.req, c.d)
		if got != c.want || !until.Equal(c.wantUntil) {
			t.Errorf("%s: got (%v, %v), want (%v, %v)", c.name, got, until, c.want, c.wantUntil)
		}
	}
}

// The compiler applies a policy's authentication requirement, and the
// snapshot expires when the first allowed device's authentication runs out.
func TestResourcePolicyAuthentication_Compile(t *testing.T) {
	pool, ctx := lifecycleTestPool(t)
	f := newLifecycleFixture(t, pool, ctx, "polauth")
	rnID, _ := mustInsertRNWithConnector(t, ctx, pool, f.wsID, "rn-auth", "td-auth", "10.43.0.1")
	gated := mustInsertResource(t, ctx, pool, f.wsID, rnID, "res-gated", "10.43.0.10", 443)
	open := mustInsertResource(t, ctx, pool, f.wsID, rnID, "res-open", "10.43.0.11", 443)
	mustAssignResourceToGroup(t, ctx, pool, f.wsID, gated, f.grpID)
	mustAssignResourceToGroup(t, ctx, pool, f.wsID, open, f.grpID)
	userID := mustInsertUser(t, ctx, pool, f.wsID)
	mustAddGroupMember(t, ctx, pool, f.grpID, userID)
	spiffe := "spiffe://td/client/" + userID
	deviceID := mustInsertClientDevice(t, ctx, pool, f.wsID, userID, spiffe)

	ws := uuid.MustParse(f.wsID)
	pstore := posture.NewStore(pool)
	store := NewStore(pool)
	notifier := NewNotifier(NewSnapshotCache())

	strict, err := pstore.CreateResourcePolicy(ctx, ws, "Strict", nil)
	if err != nil {
		t.Fatalf("create policy: %v", err)
	}
	if _, err := pstore.SetResourcePolicy(ctx, ws, uuid.MustParse(gated), strict.ID); err != nil {
		t.Fatalf("assign policy: %v", err)
	}
	setAuth := func(minutes *int, mfa bool) {
		t.Helper()
		if _, err := pstore.SetResourcePolicyAuthentication(ctx, ws, strict.ID, minutes, mfa); err != nil {
			t.Fatalf("set authentication: %v", err)
		}
	}
	setDevice := func(authAgo, mfaAgo, reauthAgo *time.Duration) {
		t.Helper()
		ts := func(d *time.Duration) *time.Time {
			if d == nil {
				return nil
			}
			v := time.Now().Add(-*d)
			return &v
		}
		if _, err := pool.Exec(ctx,
			`UPDATE client_devices SET session_auth_at = $2, session_mfa_at = $3, reauth_required_at = $4 WHERE id = $1`,
			deviceID, ts(authAgo), ts(mfaAgo), ts(reauthAgo)); err != nil {
			t.Fatalf("update device: %v", err)
		}
	}
	compile := func() (gatedAllowed, openAllowed bool, validUntil time.Time) {
		t.Helper()
		compiled, err := CompileACLSnapshot(ctx, store, pstore, notifier, f.wsID)
		if err != nil {
			t.Fatalf("compile: %v", err)
		}
		for _, e := range compiled.Snapshot.Entries {
			switch e.ResourceId {
			case gated:
				gatedAllowed = contains(e.AllowedSpiffeIds, spiffe)
			case open:
				openAllowed = contains(e.AllowedSpiffeIds, spiffe)
			}
		}
		return gatedAllowed, openAllowed, compiled.ValidUntil
	}
	d := func(v time.Duration) *time.Duration { return &v }
	sixty := 60

	// Device-only (the default): no authentication data needed, no expiry.
	if g, o, until := compile(); !g || !o || !until.IsZero() {
		t.Fatalf("device-only: gated=%v open=%v validUntil=%v, want true true zero", g, o, until)
	}

	// A frequency, device never authenticated: gated resource only is denied.
	setAuth(&sixty, false)
	if g, o, _ := compile(); g || !o {
		t.Fatalf("never authenticated: gated=%v open=%v, want false true", g, o)
	}

	// Authenticated 10 minutes ago: allowed, snapshot valid ~50 more minutes.
	setDevice(d(10*time.Minute), nil, nil)
	g, o, until := compile()
	if !g || !o {
		t.Fatalf("fresh: gated=%v open=%v, want true true", g, o)
	}
	if left := time.Until(until); left < 49*time.Minute || left > 51*time.Minute {
		t.Fatalf("fresh: snapshot valid for %v, want ~50m", left)
	}

	// Authenticated 2 hours ago: denied.
	setDevice(d(2*time.Hour), nil, nil)
	if g, _, _ := compile(); g {
		t.Fatal("stale authentication still allowed")
	}

	// MFA required: fresh authentication alone is not enough.
	setAuth(&sixty, true)
	setDevice(d(time.Minute), nil, nil)
	if g, _, _ := compile(); g {
		t.Fatal("MFA required but a session without MFA was allowed")
	}
	setDevice(d(time.Minute), d(40*time.Minute), nil)
	g, _, until = compile()
	if !g {
		t.Fatal("fresh authentication and MFA denied")
	}
	if left := time.Until(until); left < 19*time.Minute || left > 21*time.Minute {
		t.Fatalf("MFA: snapshot valid for %v, want ~20m (the MFA runs out first)", left)
	}

	// Risk demand newer than the authentication: denied until re-authenticated.
	setDevice(d(10*time.Minute), d(10*time.Minute), d(5*time.Minute))
	if g, o, _ := compile(); g || !o {
		t.Fatalf("risk demand: gated=%v open=%v, want false true (device-only policies are unaffected)", g, o)
	}
	setDevice(d(time.Minute), d(time.Minute), d(5*time.Minute))
	if g, _, _ := compile(); !g {
		t.Fatal("re-authenticated after the risk demand but still denied")
	}

	// The store refuses MFA without a frequency and out-of-range frequencies.
	if _, err := pstore.SetResourcePolicyAuthentication(ctx, ws, strict.ID, nil, true); err == nil {
		t.Fatal("MFA without a frequency accepted")
	}
	tooShort, tooLong := 59, 31*24*60+1
	for _, m := range []*int{&tooShort, &tooLong} {
		if _, err := pstore.SetResourcePolicyAuthentication(ctx, ws, strict.ID, m, false); err == nil {
			t.Fatalf("frequency %d accepted", *m)
		}
	}
}
