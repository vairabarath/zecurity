package posture

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/yourorg/ztna/controller/internal/geoip"
)

func TestNormalizeCountryRule(t *testing.T) {
	got, err := NormalizeCountryRule(CountryRule{Allow: []string{" in", "us", "IN"}, Deny: []string{"cn"}})
	if err != nil {
		t.Fatal(err)
	}
	if want := (CountryRule{Allow: []string{"IN", "US"}, Deny: []string{"CN"}}); !equalRule(got, want) {
		t.Fatalf("normalized %+v, want %+v", got, want)
	}
	if got, err := NormalizeCountryRule(CountryRule{Deny: []string{"RU"}}); err != nil || len(got.Allow) != 0 {
		t.Fatalf("deny-only rule (G-8) must be allowed: %+v %v", got, err)
	}
	for name, bad := range map[string]CountryRule{
		"both empty":   {},
		"bad code":     {Allow: []string{"IND"}},
		"digit":        {Allow: []string{"1N"}},
		"in both":      {Allow: []string{"IN"}, Deny: []string{"in"}},
		"empty string": {Deny: []string{""}},
	} {
		if _, err := NormalizeCountryRule(bad); !errors.Is(err, ErrInvalidCountryRule) {
			t.Errorf("%s: err = %v, want ErrInvalidCountryRule", name, err)
		}
	}
}

func equalRule(a, b CountryRule) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}

func TestCountryRuleAllows(t *testing.T) {
	allowList := CountryRule{Allow: []string{"IN", "US"}}
	denyList := CountryRule{Deny: []string{"CN", "RU"}}
	both := CountryRule{Allow: []string{"IN"}, Deny: []string{"CN"}}
	for _, tc := range []struct {
		rule    CountryRule
		country string
		want    bool
	}{
		{allowList, "IN", true},
		{allowList, "GB", false},
		{denyList, "GB", true},
		{denyList, "RU", false},
		{both, "IN", true},
		{both, "CN", false},
		{both, "GB", false},
		{allowList, "", false}, // unknown never passes (G-4)
		{denyList, "", false},
	} {
		if got := tc.rule.Allows(tc.country); got != tc.want {
			t.Errorf("%+v allows %q = %v, want %v", tc.rule, tc.country, got, tc.want)
		}
	}
}

func TestEvaluateProfileCountry(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	report := uuid.New()
	profile := Profile{ID: uuid.New(), Revision: 1, AllowedPlatforms: []string{"linux"}}
	rule := &CountryRule{Allow: []string{"IN"}, Deny: []string{"CN"}}
	req := []Requirement{{CheckID: CheckNetworkCountry, Countries: rule}}
	seen := func(country string, ago time.Duration) *NetworkObservation {
		return &NetworkObservation{Country: country, ObservedAt: now.Add(-ago)}
	}
	for _, tc := range []struct {
		name   string
		obs    *NetworkObservation
		req    []Requirement
		ok     bool
		reason string
	}{
		{"allowed", seen("IN", time.Minute), req, true, "all requirements satisfied"},
		{"denied", seen("CN", time.Minute), req, false, "check zecurity.network.country: CN is denied"},
		{"not allowed", seen("GB", time.Minute), req, false, "check zecurity.network.country: GB is not allowed"},
		{"unknown", seen("", time.Minute), req, false, "check zecurity.network.country: device location unknown (no country for its address)"},
		{"stale", seen("IN", MaxReportAge+time.Second), req, false, "check zecurity.network.country: device location is stale"},
		{"never seen", nil, req, false, "check zecurity.network.country: device not seen yet"},
		{"no rule", seen("IN", time.Minute), []Requirement{{CheckID: CheckNetworkCountry}}, false, "check zecurity.network.country has no country rule"},
	} {
		obs := map[string]Observation{CheckNetworkCountry: countryObservation(report, tc.obs, now)}
		got := EvaluateProfile(now, now.Add(-time.Minute), profile, "linux", tc.req, obs)
		if got.Satisfied != tc.ok || got.Reason != tc.reason {
			t.Errorf("%s: satisfied=%v reason=%q, want %v %q", tc.name, got.Satisfied, got.Reason, tc.ok, tc.reason)
		}
	}
}

// A client can never report the server-side country check.
func TestClientCannotReportCountry(t *testing.T) {
	if IsServerCheck(CheckNetworkCountry) != true {
		t.Fatal("country check must be server-side")
	}
	now := time.Now().UTC()
	rep, err := ValidateReport(now, ReportInput{
		ReportID:   uuid.NewString(),
		ReportedAt: now,
		Checks: []CheckInput{
			{CheckID: CheckNetworkCountry, Status: ObservationStatusPass, Detail: "IN"},
			{CheckID: CheckFirewall, Status: ObservationStatusPass},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range rep.Observations {
		if o.CheckID == CheckNetworkCountry {
			t.Fatal("client-reported country observation was accepted")
		}
	}
}

// DB-backed (must run, not skip, in the gate): stored rules, observations,
// change detection, evaluation and cross-workspace safety.
func TestCountryGeofenceIntegration(t *testing.T) {
	adminDSN := os.Getenv("PKI_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("PKI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	dbName := postureTestDBName(t)
	adminPool := postureTestPool(t, ctx, adminDSN)
	defer adminPool.Close()
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	defer func() { _, _ = adminPool.Exec(ctx, "DROP DATABASE IF EXISTS "+dbName) }()
	testDSN, err := postureTestDSN(adminDSN, dbName)
	if err != nil {
		t.Fatal(err)
	}
	pool := postureTestPool(t, ctx, testDSN)
	defer pool.Close()
	if err := applyPostureTestMigrations(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	ws := insertPostureTestWorkspace(t, ctx, pool, "geo")
	otherWS := insertPostureTestWorkspace(t, ctx, pool, "geo-other")
	device := insertPostureTestDevice(t, ctx, pool, ws, "geo")
	store := NewStore(pool)
	if err := store.InsertReport(ctx, ws, device, Report{
		ReportID:      uuid.NewString(),
		ClientVersion: "test-client",
		OSInfo:        json.RawMessage(`{"name":"linux","version":"test"}`),
		ReportedAt:    time.Now().UTC(),
		Observations:  []Observation{{CheckID: CheckFirewall, Status: ObservationStatusPass}},
	}); err != nil {
		t.Fatalf("insert report: %v", err)
	}

	profile, err := store.CreateProfile(ctx, ws, "India only", true)
	if err != nil {
		t.Fatal(err)
	}
	// ADR-031: profiles start with no platform; these devices are Linux.
	if _, err := store.SetAllowedPlatforms(ctx, ws, profile.ID, []string{"linux"}); err != nil {
		t.Fatalf("allow linux: %v", err)
	}
	rule, _ := NormalizeCountryRule(CountryRule{Allow: []string{"IN"}, Deny: []string{"CN"}})
	if err := store.AddRequirement(ctx, ws, profile.ID, Requirement{CheckID: CheckNetworkCountry, Countries: &rule}); err != nil {
		t.Fatalf("add country requirement: %v", err)
	}
	reqs, err := store.ListRequirements(ctx, ws, profile.ID)
	if err != nil || len(reqs) != 1 || reqs[0].Countries == nil || !equalRule(*reqs[0].Countries, rule) {
		t.Fatalf("stored rule round trip: %+v %v", reqs, err)
	}

	notifier := &postureTestNotifier{}
	evaluator := NewEvaluator(store, notifier)
	evaluate := func() ProfileResult {
		t.Helper()
		res, err := evaluator.EvaluateDevice(ctx, ws, device)
		if err != nil || len(res) != 1 {
			t.Fatalf("evaluate: %v %v", res, err)
		}
		return res[0]
	}
	record := func(country string) bool {
		t.Helper()
		changed, err := store.RecordNetworkObservation(ctx, ws, device, geoip.Lookup{
			Country: country, Prefix: netip.MustParsePrefix("49.36.200.0/24"),
		}, time.Now().UTC())
		if err != nil {
			t.Fatalf("record observation: %v", err)
		}
		return changed
	}

	if got := evaluate(); got.Satisfied || got.Reason != "check zecurity.network.country: device not seen yet" {
		t.Fatalf("never seen: %+v", got)
	}
	if !record("IN") {
		t.Fatal("first sighting must report a change")
	}
	if record("IN") {
		t.Fatal("same country must not report a change")
	}
	calls := notifier.calls
	if got := evaluate(); !got.Satisfied {
		t.Fatalf("seen in IN: %+v", got)
	}
	if notifier.calls != calls+1 {
		t.Fatalf("FAIL→PASS must notify the ACL once, got %d", notifier.calls-calls)
	}
	if !record("CN") {
		t.Fatal("IN→CN must report a change")
	}
	if got := evaluate(); got.Satisfied || got.Reason != "check zecurity.network.country: CN is denied" {
		t.Fatalf("seen in CN: %+v", got)
	}
	if !record("") {
		t.Fatal("CN→unknown must report a change")
	}
	if got := evaluate(); got.Satisfied {
		t.Fatalf("unknown country must deny: %+v", got)
	}

	obs, err := store.LatestNetworkObservation(ctx, ws, device)
	if err != nil || obs.NetworkPrefix.String() != "49.36.200.0/24" || obs.Country != "" {
		t.Fatalf("stored observation: %+v %v", obs, err)
	}
	// Only the /24 is stored, never the full address (G-6).
	var stored string
	if err := pool.QueryRow(ctx, `SELECT network_prefix::text FROM device_network_observations WHERE device_id = $1`, device).Scan(&stored); err != nil || stored != "49.36.200.0/24" {
		t.Fatalf("stored prefix %q %v", stored, err)
	}

	// A stale sighting is unknown even if the country is allowed.
	record("IN")
	if _, err := pool.Exec(ctx, `UPDATE device_network_observations SET observed_at = NOW() - INTERVAL '11 minutes' WHERE device_id = $1`, device); err != nil {
		t.Fatal(err)
	}
	if got := evaluate(); got.Satisfied || got.Reason != "check zecurity.network.country: device location is stale" {
		t.Fatalf("stale: %+v", got)
	}

	// G-10: replace the lists atomically; revision bumps, old rule returned.
	before, err := store.GetProfile(ctx, ws, profile.ID)
	if err != nil {
		t.Fatal(err)
	}
	newRule, _ := NormalizeCountryRule(CountryRule{Deny: []string{"CN", "RU"}})
	old, err := store.UpdateCountryRule(ctx, ws, profile.ID, newRule)
	if err != nil || !equalRule(old, rule) {
		t.Fatalf("update: old=%+v err=%v", old, err)
	}
	after, _ := store.GetProfile(ctx, ws, profile.ID)
	if after.Revision != before.Revision+1 {
		t.Fatalf("revision %d -> %d, want +1", before.Revision, after.Revision)
	}
	reqs, _ = store.ListRequirements(ctx, ws, profile.ID)
	if len(reqs) != 1 || !equalRule(*reqs[0].Countries, newRule) {
		t.Fatalf("after update: %+v", reqs)
	}
	record("GB")
	if got := evaluate(); !got.Satisfied {
		t.Fatalf("deny-only rule must admit GB: %+v", got)
	}
	plain, _ := store.CreateProfile(ctx, ws, "No country check", true)
	if _, err := store.UpdateCountryRule(ctx, ws, plain.ID, newRule); !errors.Is(err, ErrNotFound) {
		t.Fatalf("profile without a country check: %v", err)
	}
	if _, err := store.UpdateCountryRule(ctx, otherWS, profile.ID, newRule); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-workspace update: %v", err)
	}

	// Another workspace can't write this device's observation.
	if _, err := store.RecordNetworkObservation(ctx, otherWS, device, geoip.Lookup{Country: "IN"}, time.Now()); err == nil {
		t.Fatal("cross-workspace observation was accepted")
	}
	if _, err := store.LatestNetworkObservation(ctx, otherWS, device); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-workspace read: %v", err)
	}
}
