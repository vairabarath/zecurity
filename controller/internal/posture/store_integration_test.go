package posture

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestStoreIntegration(t *testing.T) {
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

	workspaceA := insertPostureTestWorkspace(t, ctx, pool, "posture-a")
	workspaceB := insertPostureTestWorkspace(t, ctx, pool, "posture-b")
	deviceA := insertPostureTestDevice(t, ctx, pool, workspaceA, "a")
	deviceB := insertPostureTestDevice(t, ctx, pool, workspaceB, "b")
	resourceB := insertPostureTestResource(t, ctx, pool, workspaceB, "b")
	store := NewStore(pool)

	reportKey := uuid.NewString()
	report := Report{
		ReportID:      reportKey,
		ClientVersion: "test-client",
		OSInfo:        json.RawMessage(`{"name":"linux","version":"test"}`),
		ReportedAt:    time.Now().UTC(),
		Observations: []Observation{{
			CheckID: "linux.disk.encryption",
			Status:  "PASS",
		}},
	}
	if err := store.InsertReport(ctx, workspaceA, deviceA, report); err != nil {
		t.Fatalf("insert report: %v", err)
	}
	if err := store.InsertReport(ctx, workspaceA, deviceA, report); !errors.Is(err, ErrDuplicateReport) {
		t.Fatalf("duplicate report error = %v, want %v", err, ErrDuplicateReport)
	}

	latestReport, err := store.LatestReport(ctx, workspaceA, deviceA)
	if err != nil {
		t.Fatalf("latest report: %v", err)
	}
	observations, err := store.ListObservations(ctx, latestReport.ID)
	if err != nil {
		t.Fatalf("list observations: %v", err)
	}
	if len(observations) != 1 || observations[0].CheckID != "linux.disk.encryption" {
		t.Fatalf("observations = %#v", observations)
	}

	profile, err := store.CreateProfile(ctx, workspaceA, "Managed Linux")
	if err != nil {
		t.Fatalf("create profile: %v", err)
	}
	emptyProfile, err := store.CreateProfile(ctx, workspaceA, "Empty enforce guard")
	if err != nil {
		t.Fatalf("create empty profile: %v", err)
	}
	if _, err := store.UpdateProfileMode(ctx, workspaceA, emptyProfile.ID, ModeEnforce); !errors.Is(err, ErrEmptyEnforceProfile) {
		t.Fatalf("enforce empty profile error = %v, want %v", err, ErrEmptyEnforceProfile)
	}
	if err := store.AddRequirement(ctx, workspaceA, profile.ID, Requirement{
		CheckID: "linux.disk.encryption",
	}); err != nil {
		t.Fatalf("add requirement: %v", err)
	}

	profile, err = store.GetProfile(ctx, workspaceA, profile.ID)
	if err != nil {
		t.Fatalf("reload profile: %v", err)
	}
	if profile.Revision != 2 {
		t.Fatalf("profile revision = %d, want 2", profile.Revision)
	}
	if _, err := store.UpdateProfileMode(ctx, workspaceA, profile.ID, ModeEnforce); err != nil {
		t.Fatalf("enforce profile: %v", err)
	}
	if err := store.RemoveRequirement(
		ctx,
		workspaceA,
		profile.ID,
		"linux.disk.encryption",
	); !errors.Is(err, ErrLastRequirement) {
		t.Fatalf("remove last enforced requirement error = %v, want %v", err, ErrLastRequirement)
	}
	requirements, err := store.ListRequirements(ctx, workspaceA, profile.ID)
	if err != nil {
		t.Fatalf("list requirements: %v", err)
	}
	if len(requirements) != 1 {
		t.Fatalf("requirements len = %d, want 1", len(requirements))
	}

	if _, err := store.CreateResourceBinding(
		ctx,
		workspaceA,
		resourceB,
		profile.ID,
	); !errors.Is(err, ErrWorkspaceMismatch) {
		t.Fatalf("cross-workspace binding error = %v, want %v", err, ErrWorkspaceMismatch)
	}

	reason := "all required checks passed"
	if err := store.UpsertEvaluation(
		ctx,
		workspaceA,
		deviceA,
		profile.ID,
		profile.Revision,
		true,
		&reason,
		&latestReport.ID,
	); err != nil {
		t.Fatalf("upsert evaluation: %v", err)
	}

	updatedReason := "re-evaluated"
	if err := store.UpsertEvaluation(
		ctx,
		workspaceA,
		deviceA,
		profile.ID,
		profile.Revision,
		true,
		&updatedReason,
		&latestReport.ID,
	); err != nil {
		t.Fatalf("repeat upsert evaluation: %v", err)
	}

	evaluations, err := store.EvaluationsForDevices(
		ctx,
		workspaceA,
		[]uuid.UUID{deviceA},
	)
	if err != nil {
		t.Fatalf("batch evaluations: %v", err)
	}
	if len(evaluations[deviceA]) != 1 {
		t.Fatalf("device evaluations len = %d, want 1", len(evaluations[deviceA]))
	}
	got := evaluations[deviceA][0]
	if got.Reason == nil || *got.Reason != updatedReason || got.ReportReceivedAt == nil {
		t.Fatalf("batch evaluation = %#v", got)
	}

	if err := store.UpsertEvaluation(
		ctx,
		workspaceA,
		deviceB,
		profile.ID,
		profile.Revision,
		true,
		nil,
		nil,
	); !errors.Is(err, ErrWorkspaceMismatch) {
		t.Fatalf("cross-workspace evaluation error = %v, want %v", err, ErrWorkspaceMismatch)
	}
}

func postureTestPool(t *testing.T, ctx context.Context, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping: %v", err)
	}
	return pool
}

func postureTestDBName(t *testing.T) string {
	name := strings.ToLower(t.Name())
	name = strings.ReplaceAll(name, "/", "_")
	name = strings.ReplaceAll(name, " ", "_")
	return fmt.Sprintf("%s_%d_%d", name, os.Getpid(), time.Now().UnixNano())
}

func postureTestDSN(dsn, dbName string) (string, error) {
	parsed, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("parse DSN: %w", err)
	}
	parsed.Path = "/" + dbName
	return parsed.String(), nil
}

func applyPostureTestMigrations(ctx context.Context, pool *pgxpool.Pool) error {
	dir, err := filepath.Abs(filepath.Join("..", "..", "migrations"))
	if err != nil {
		return fmt.Errorf("resolve migrations: %w", err)
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil {
		return fmt.Errorf("glob migrations: %w", err)
	}
	sort.Strings(files)
	for _, filename := range files {
		contents, err := os.ReadFile(filename)
		if err != nil {
			return fmt.Errorf("read %s: %w", filename, err)
		}
		if _, err := pool.Exec(ctx, string(contents)); err != nil {
			return fmt.Errorf("execute %s: %w", filepath.Base(filename), err)
		}
	}
	return nil
}

func insertPostureTestWorkspace(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	suffix string,
) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	slug := fmt.Sprintf("posture-%s-%d", suffix, time.Now().UnixNano())
	if err := pool.QueryRow(
		ctx,
		`INSERT INTO workspaces (slug, name, status, trust_domain)
		 VALUES ($1, $1, 'active', $1 || '.test')
		 RETURNING id`,
		slug,
	).Scan(&id); err != nil {
		t.Fatalf("insert workspace: %v", err)
	}
	return id
}

func insertPostureTestDevice(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	workspaceID uuid.UUID,
	suffix string,
) uuid.UUID {
	t.Helper()
	var userID uuid.UUID
	if err := pool.QueryRow(
		ctx,
		`INSERT INTO users (
		     tenant_id, email, provider, provider_sub, role, status
		 )
		 VALUES ($1, $2, 'test', $3, 'member', 'active')
		 RETURNING id`,
		workspaceID,
		fmt.Sprintf("%s@example.test", suffix),
		uuid.NewString(),
	).Scan(&userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}

	var deviceID uuid.UUID
	if err := pool.QueryRow(
		ctx,
		`INSERT INTO client_devices (user_id, workspace_id, name, os)
		 VALUES ($1, $2, $3, 'linux')
		 RETURNING id`,
		userID,
		workspaceID,
		"device-"+suffix,
	).Scan(&deviceID); err != nil {
		t.Fatalf("insert client device: %v", err)
	}
	return deviceID
}

func insertPostureTestResource(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	workspaceID uuid.UUID,
	suffix string,
) uuid.UUID {
	t.Helper()
	var networkID uuid.UUID
	if err := pool.QueryRow(
		ctx,
		`INSERT INTO remote_networks (tenant_id, name, location)
		 VALUES ($1, $2, 'other')
		 RETURNING id`,
		workspaceID,
		"network-"+suffix,
	).Scan(&networkID); err != nil {
		t.Fatalf("insert remote network: %v", err)
	}

	var connectorID uuid.UUID
	if err := pool.QueryRow(
		ctx,
		`INSERT INTO connectors (tenant_id, remote_network_id, name)
		 VALUES ($1, $2, $3)
		 RETURNING id`,
		workspaceID,
		networkID,
		"connector-"+suffix,
	).Scan(&connectorID); err != nil {
		t.Fatalf("insert connector: %v", err)
	}

	var shieldID uuid.UUID
	if err := pool.QueryRow(
		ctx,
		`INSERT INTO shields (tenant_id, remote_network_id, connector_id, name)
		 VALUES ($1, $2, $3, $4)
		 RETURNING id`,
		workspaceID,
		networkID,
		connectorID,
		"shield-"+suffix,
	).Scan(&shieldID); err != nil {
		t.Fatalf("insert shield: %v", err)
	}

	var resourceID uuid.UUID
	if err := pool.QueryRow(
		ctx,
		`INSERT INTO resources (
		     tenant_id, remote_network_id, shield_id, name, host, port_from, port_to
		 )
		 VALUES ($1, $2, $3, $4, '127.0.0.1', 443, 443)
		 RETURNING id`,
		workspaceID,
		networkID,
		shieldID,
		"resource-"+suffix,
	).Scan(&resourceID); err != nil {
		t.Fatalf("insert resource: %v", err)
	}
	return resourceID
}
