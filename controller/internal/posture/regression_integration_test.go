package posture

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Review fix (PENDING-06 M-9): the regression risk signal fires when the
// device itself stops satisfying a required profile, never because an admin
// tightened the profile (a new revision).
func TestRegressionHook_DeviceChangeOnly(t *testing.T) {
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
	defer func() { _, _ = adminPool.Exec(ctx, "DROP DATABASE IF EXISTS "+dbName+" WITH (FORCE)") }()
	testDSN, err := postureTestDSN(adminDSN, dbName)
	if err != nil {
		t.Fatal(err)
	}
	pool := postureTestPool(t, ctx, testDSN)
	defer pool.Close()
	if err := applyPostureTestMigrations(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	ws := insertPostureTestWorkspace(t, ctx, pool, "regress")
	device := insertPostureTestDevice(t, ctx, pool, ws, "r")
	store := NewStore(pool)

	profile, err := store.CreateProfile(ctx, ws, "Regress", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetAllowedPlatforms(ctx, ws, profile.ID, []string{"linux"}); err != nil {
		t.Fatal(err)
	}
	if err := store.AddRequirement(ctx, ws, profile.ID, Requirement{CheckID: "linux.disk.encryption"}); err != nil {
		t.Fatal(err)
	}
	policy, err := store.CreateResourcePolicy(ctx, ws, "Regress policy", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetResourcePolicyProfiles(ctx, ws, policy.ID, []uuid.UUID{profile.ID}); err != nil {
		t.Fatal(err)
	}

	eval := NewEvaluator(store, &postureTestNotifier{})
	fired := 0
	eval.SetRegressionHook(func(context.Context, uuid.UUID, uuid.UUID) { fired++ })

	report := func(status ObservationStatus) {
		t.Helper()
		if err := store.InsertReport(ctx, ws, device, Report{
			ReportID:      uuid.NewString(),
			ClientVersion: "test-client",
			OSInfo:        json.RawMessage(`{"name":"linux","version":"test"}`),
			ReportedAt:    time.Now().UTC(),
			Observations:  []Observation{{CheckID: "linux.disk.encryption", Status: status}},
		}); err != nil {
			t.Fatalf("insert report: %v", err)
		}
		if err := eval.ReevaluateWorkspace(ctx, ws); err != nil {
			t.Fatalf("re-evaluate: %v", err)
		}
	}

	report(ObservationStatusPass)
	if fired != 0 {
		t.Fatalf("first pass fired %d", fired)
	}
	report(ObservationStatusFail)
	if fired != 1 {
		t.Fatalf("device regression fired %d, want 1", fired)
	}

	report(ObservationStatusPass)
	// The admin tightens the profile; the device now fails it.
	if err := store.AddRequirement(ctx, ws, profile.ID, Requirement{CheckID: CheckFirewall}); err != nil {
		t.Fatal(err)
	}
	if err := eval.ReevaluateWorkspace(ctx, ws); err != nil {
		t.Fatal(err)
	}
	if ev, err := store.LatestEvaluation(ctx, ws, device, profile.ID); err != nil || ev.Satisfied {
		t.Fatalf("after tightening: satisfied=%v err=%v, want failing", ev.Satisfied, err)
	}
	if fired != 1 {
		t.Fatalf("profile edit fired the regression hook (count %d, want 1)", fired)
	}
}
