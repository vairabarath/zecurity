package posture

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

// ADR-030: a profile requiring zecurity.device.approved is satisfied only by
// an admin's approval in client_devices, never by what the client reports.
func TestEvaluatorDeviceApproval(t *testing.T) {
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
	defer func() {
		if _, err := adminPool.Exec(ctx, "DROP DATABASE IF EXISTS "+dbName); err != nil {
			t.Logf("drop test database: %v", err)
		}
	}()
	testDSN, err := postureTestDSN(adminDSN, dbName)
	if err != nil {
		t.Fatalf("build test database DSN: %v", err)
	}
	pool := postureTestPool(t, ctx, testDSN)
	defer pool.Close()
	if err := applyPostureTestMigrations(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	ws := insertPostureTestWorkspace(t, ctx, pool, "approval")
	device := insertPostureTestDevice(t, ctx, pool, ws, "approval")
	store := NewStore(pool)

	// The report claims approval, as a modified client could; the store is
	// written directly to bypass ValidateReport.
	if err := store.InsertReport(ctx, ws, device, Report{
		ReportID:      uuid.NewString(),
		ClientVersion: "test-client",
		OSInfo:        json.RawMessage(`{"name":"linux","version":"test"}`),
		ReportedAt:    time.Now().UTC(),
		Observations: []Observation{
			{CheckID: CheckDeviceApproved, Status: ObservationStatusPass},
			{CheckID: CheckFirewall, Status: ObservationStatusPass},
		},
	}); err != nil {
		t.Fatalf("insert report: %v", err)
	}

	company, err := store.CreateProfile(ctx, ws, "Company devices", true)
	if err != nil {
		t.Fatalf("create profile: %v", err)
	}
	if err := store.AddRequirement(ctx, ws, company.ID, Requirement{CheckID: CheckDeviceApproved}); err != nil {
		t.Fatalf("add approval requirement: %v", err)
	}
	byod, err := store.CreateProfile(ctx, ws, "BYOD", true)
	if err != nil {
		t.Fatalf("create profile: %v", err)
	}
	if err := store.AddRequirement(ctx, ws, byod.ID, Requirement{CheckID: CheckFirewall}); err != nil {
		t.Fatalf("add firewall requirement: %v", err)
	}

	notifier := &postureTestNotifier{}
	evaluator := NewEvaluator(store, notifier)
	satisfied := func(t *testing.T) map[uuid.UUID]ProfileResult {
		t.Helper()
		results, err := evaluator.EvaluateDevice(ctx, ws, device)
		if err != nil {
			t.Fatalf("evaluate: %v", err)
		}
		byProfile := map[uuid.UUID]ProfileResult{}
		for _, r := range results {
			byProfile[r.ProfileID] = r
		}
		return byProfile
	}
	exec := func(t *testing.T, sql string) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, device); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}

	got := satisfied(t)
	if got[company.ID].Satisfied {
		t.Fatalf("unapproved device satisfied the company profile from a client claim: %#v", got[company.ID])
	}
	if got[company.ID].Reason != "check zecurity.device.approved: FAIL: device not approved by an admin" {
		t.Fatalf("company reason = %q", got[company.ID].Reason)
	}
	if !got[byod.ID].Satisfied {
		t.Fatalf("BYOD profile must not depend on approval: %#v", got[byod.ID])
	}

	exec(t, `UPDATE client_devices SET approved_at = NOW() WHERE id = $1`)
	calls := notifier.calls
	if got := satisfied(t); !got[company.ID].Satisfied {
		t.Fatalf("approved device must satisfy the company profile: %#v", got[company.ID])
	}
	if notifier.calls != calls+1 {
		t.Fatalf("approval transition must notify the ACL once, got %d calls", notifier.calls-calls)
	}

	exec(t, `UPDATE client_devices SET revoked_at = NOW() WHERE id = $1`)
	if got := satisfied(t); got[company.ID].Satisfied {
		t.Fatalf("a revoked device is never approved: %#v", got[company.ID])
	}

	exec(t, `UPDATE client_devices SET revoked_at = NULL, approved_at = NULL WHERE id = $1`)
	if got := satisfied(t); got[company.ID].Satisfied {
		t.Fatalf("un-approved device must fail the company profile: %#v", got[company.ID])
	}

	// Another workspace's lookup never sees this device's approval.
	exec(t, `UPDATE client_devices SET approved_at = NOW() WHERE id = $1`)
	if ok, err := store.DeviceApproved(ctx, uuid.New(), device); err != nil || ok {
		t.Fatalf("cross-workspace DeviceApproved = %v, %v; want false, nil", ok, err)
	}
}
