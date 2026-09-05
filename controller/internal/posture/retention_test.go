package posture

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestStoreDeleteExpiredReports(t *testing.T) {
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

	workspaceID := insertPostureTestWorkspace(t, ctx, pool, "retention")
	deviceID := insertPostureTestDevice(t, ctx, pool, workspaceID, "retention")
	store := NewStore(pool)
	now := time.Now().UTC()

	oldReport := Report{
		ReportID:      uuid.NewString(),
		ClientVersion: "1.0.0",
		OSInfo:        json.RawMessage(`{"name":"linux"}`),
		ReportedAt:    now.Add(-40 * 24 * time.Hour),
	}
	if err := store.InsertReport(ctx, workspaceID, deviceID, oldReport); err != nil {
		t.Fatalf("insert old report: %v", err)
	}
	oldStored, err := store.ReportByClientID(ctx, oldReport.ReportID)
	if err != nil {
		t.Fatalf("load old report: %v", err)
	}

	recentReport := Report{
		ReportID:      uuid.NewString(),
		ClientVersion: "1.0.0",
		OSInfo:        json.RawMessage(`{"name":"linux"}`),
		ReportedAt:    now.Add(-10 * 24 * time.Hour),
	}
	if err := store.InsertReport(ctx, workspaceID, deviceID, recentReport); err != nil {
		t.Fatalf("insert recent report: %v", err)
	}
	recentStored, err := store.ReportByClientID(ctx, recentReport.ReportID)
	if err != nil {
		t.Fatalf("load recent report: %v", err)
	}

	// InsertReport records received_at at ingestion time, so set deterministic
	// ages for the retention boundary under test.
	for _, update := range []struct {
		id       uuid.UUID
		received time.Time
	}{
		{oldStored.ID, now.Add(-40 * 24 * time.Hour)},
		{recentStored.ID, now.Add(-10 * 24 * time.Hour)},
	} {
		if _, err := pool.Exec(
			ctx,
			`UPDATE device_posture_reports SET received_at = $1 WHERE id = $2`,
			update.received,
			update.id,
		); err != nil {
			t.Fatalf("set report received_at: %v", err)
		}
	}

	profile, err := store.CreateProfile(ctx, workspaceID, "retention-profile", true)
	if err != nil {
		t.Fatalf("create profile: %v", err)
	}
	if err := store.UpsertEvaluation(
		ctx,
		workspaceID,
		deviceID,
		profile.ID,
		profile.Revision,
		true,
		nil,
		&oldStored.ID,
	); err != nil {
		t.Fatalf("insert evaluation referencing old report: %v", err)
	}

	deleted, err := store.DeleteExpiredReports(
		ctx,
		now.Add(-30*24*time.Hour),
		2000,
	)
	if err != nil {
		t.Fatalf("delete expired reports: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("deleted reports = %d, want 1", deleted)
	}

	if _, err := store.ReportByClientID(ctx, oldReport.ReportID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old report lookup error = %v, want ErrNotFound", err)
	}
	if _, err := store.ReportByClientID(ctx, recentReport.ReportID); err != nil {
		t.Fatalf("recent report should remain: %v", err)
	}

	evaluation, err := store.LatestEvaluation(ctx, workspaceID, deviceID, profile.ID)
	if err != nil {
		t.Fatalf("load evaluation after report deletion: %v", err)
	}
	if evaluation.ReportID != nil {
		t.Fatalf("evaluation report ID = %v, want NULL after retention deletion", evaluation.ReportID)
	}
}

func TestRetentionWorkerCleanupDeletesAllExpiredReports(t *testing.T) {
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

	workspaceID := insertPostureTestWorkspace(t, ctx, pool, "retention")
	deviceID := insertPostureTestDevice(t, ctx, pool, workspaceID, "retention")
	store := NewStore(pool)
	now := time.Now().UTC()

	worker := NewRetentionWorker(
		store,
		30*24*time.Hour,
		2,
		func() time.Time { return now },
	)

	for i := 0; i < 5; i++ {
		report := Report{
			ReportID:      uuid.NewString(),
			ClientVersion: "1.0.0",
			OSInfo:        json.RawMessage(`{"name":"linux"}`),
			ReportedAt:    now.Add(-40 * 24 * time.Hour),
		}

		if err := store.InsertReport(
			ctx,
			workspaceID,
			deviceID,
			report,
		); err != nil {
			t.Fatalf("insert report %d: %v", i, err)
		}

		// ReportByClientID(...)
		storedReport, err := store.ReportByClientID(
			ctx,
			report.ReportID,
		)
		if err != nil {
			t.Fatalf("load report %d: %v", i, err)
		}
		// UPDATE received_at
		if _, err := pool.Exec(
			ctx,
			`
			UPDATE device_posture_reports
			SET received_at = $1
			WHERE id = $2
			`,
			now.Add(-40*24*time.Hour),
			storedReport.ID,
		); err != nil {
			t.Fatalf("set received_at for report %d: %v", i, err)
		}
	}
	err = worker.cleanup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var remaining int

	err = pool.QueryRow(
		ctx,
		`
	SELECT COUNT(*)
	FROM device_posture_reports
	WHERE received_at < $1
	`,
		now.Add(-30*24*time.Hour),
	).Scan(&remaining)

	if err != nil {
		t.Fatalf("count remaining expired reports: %v", err)
	}
	if remaining != 0 {
		t.Fatalf(
			"remaining expired reports = %d, want 0",
			remaining,
		)
	}
}
func TestRetentionWorkerRunStopsOnContextCancel(t *testing.T) {
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

	store := NewStore(pool)

	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	worker := NewRetentionWorker(
		store,
		30*24*time.Hour,
		2000,
		time.Now,
	)

	done := make(chan struct{})

	go func() {
		worker.Run(runCtx)
		close(done)
	}()

	cancel()

	select {
	case <-done:
		// success

	case <-time.After(time.Second):
		t.Fatal("worker did not stop after context cancellation")
	}
}
