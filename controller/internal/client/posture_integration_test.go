package client

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	clientv1 "github.com/yourorg/ztna/controller/gen/go/proto/client/v1"
	"github.com/yourorg/ztna/controller/graph/model"
	"github.com/yourorg/ztna/controller/internal/auth"
	"github.com/yourorg/ztna/controller/internal/posture"
)

func TestReportDevicePostureIntegration(t *testing.T) {
	adminDSN := os.Getenv("PKI_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("PKI_TEST_DATABASE_URL not set")
	}

	ctx := context.Background()
	dbName := clientPostureTestDBName(t)
	adminPool := clientPostureTestPool(t, ctx, adminDSN)
	defer adminPool.Close()

	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	defer func() {
		if _, err := adminPool.Exec(ctx, "DROP DATABASE IF EXISTS "+dbName); err != nil {
			t.Logf("drop test database: %v", err)
		}
	}()

	testDSN, err := clientPostureTestDSN(adminDSN, dbName)
	if err != nil {
		t.Fatalf("build test database DSN: %v", err)
	}
	pool := clientPostureTestPool(t, ctx, testDSN)
	defer pool.Close()
	if err := applyClientPostureTestMigrations(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	workspaceA := insertClientPostureWorkspace(t, ctx, pool, "a")
	workspaceB := insertClientPostureWorkspace(t, ctx, pool, "b")
	userA := insertClientPostureUser(t, ctx, pool, workspaceA, "a")
	deviceA := insertClientPostureDevice(t, ctx, pool, workspaceA, userA, "a")
	deviceA2 := insertClientPostureDevice(t, ctx, pool, workspaceA, userA, "a2")
	deviceCrossWorkspace := insertClientPostureDevice(t, ctx, pool, workspaceB, userA, "cross")
	deviceRevoked := insertClientPostureDevice(t, ctx, pool, workspaceA, userA, "revoked")
	if _, err := pool.Exec(ctx, `UPDATE client_devices SET revoked_at = NOW() WHERE id = $1`, deviceRevoked); err != nil {
		t.Fatalf("revoke test device: %v", err)
	}

	store := posture.NewStore(pool)
	service := &Service{
		pool: pool,
		authSvc: &postureTestAuth{claims: &auth.AccessTokenClaims{
			UserID:   userA.String(),
			TenantID: workspaceA.String(),
			Role:     "member",
		}},
		postureStore:     store,
		postureEvaluator: posture.NewEvaluator(store, nil),
	}

	reportID := uuid.NewString()
	response, err := service.ReportDevicePosture(ctx, postureRequest(deviceA, reportID))
	if err != nil {
		t.Fatalf("valid report: %v", err)
	}
	if !response.GetAccepted() {
		t.Fatal("valid report was not accepted")
	}

	// Same report and device is an idempotent success, with no second row.
	response, err = service.ReportDevicePosture(ctx, postureRequest(deviceA, reportID))
	if err != nil || !response.GetAccepted() {
		t.Fatalf("same-device replay response=%v error=%v", response, err)
	}
	var reportCount int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM device_posture_reports WHERE report_id = $1`,
		reportID,
	).Scan(&reportCount); err != nil {
		t.Fatalf("count reports: %v", err)
	}
	if reportCount != 1 {
		t.Fatalf("report count = %d, want 1", reportCount)
	}

	assertPostureRPCCode(t, service, postureRequest(deviceA2, reportID), codes.PermissionDenied)
	assertPostureRPCCode(t, service, postureRequest(deviceRevoked, uuid.NewString()), codes.PermissionDenied)
	assertPostureRPCCode(t, service, postureRequest(deviceCrossWorkspace, uuid.NewString()), codes.PermissionDenied)
}

func postureRequest(deviceID uuid.UUID, reportID string) *clientv1.ReportDevicePostureRequest {
	return &clientv1.ReportDevicePostureRequest{
		AccessToken: "valid-test-token",
		DeviceId:    deviceID.String(),
		Report: &clientv1.DevicePostureReport{
			ReportId:      reportID,
			ReportedAt:    time.Now().Unix(),
			ClientVersion: "test-client",
			OsName:        "linux",
			OsVersion:     "test",
			Checks: []*clientv1.PostureCheck{{
				CheckId: posture.CheckLUKS,
				Status:  clientv1.CheckStatus_PASS,
				Detail:  "encrypted",
			}},
		},
	}
}

func assertPostureRPCCode(
	t *testing.T,
	service *Service,
	request *clientv1.ReportDevicePostureRequest,
	want codes.Code,
) {
	t.Helper()
	_, err := service.ReportDevicePosture(context.Background(), request)
	if status.Code(err) != want {
		t.Fatalf("RPC code = %s error=%v, want %s", status.Code(err), err, want)
	}
}

type postureTestAuth struct {
	claims *auth.AccessTokenClaims
}

func (a *postureTestAuth) VerifyAccessToken(string) (*auth.AccessTokenClaims, error) {
	return a.claims, nil
}

func (*postureTestAuth) InitiateAuth(context.Context, string, *string) (*model.AuthInitPayload, error) {
	panic("not used")
}
func (*postureTestAuth) CallbackHandler() http.Handler { panic("not used") }
func (*postureTestAuth) RefreshHandler() http.Handler  { panic("not used") }
func (*postureTestAuth) LogoutHandler() http.Handler   { panic("not used") }
func (*postureTestAuth) ExchangeCode(context.Context, string, string, string) (*auth.GoogleTokenResponse, error) {
	panic("not used")
}
func (*postureTestAuth) VerifyIDToken(context.Context, string) (*auth.GoogleClaims, error) {
	panic("not used")
}
func (*postureTestAuth) IssueAccessToken(string, string, string, string) (string, int64, error) {
	panic("not used")
}
func (*postureTestAuth) IssueRefreshToken(context.Context, string) (string, error) {
	panic("not used")
}

func clientPostureTestPool(t *testing.T, ctx context.Context, dsn string) *pgxpool.Pool {
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

func clientPostureTestDBName(t *testing.T) string {
	name := strings.ToLower(t.Name())
	name = strings.ReplaceAll(name, "/", "_")
	if len(name) > 32 {
		name = name[:32]
	}
	return fmt.Sprintf("%s_%d_%d", name, os.Getpid(), time.Now().UnixNano())
}

func clientPostureTestDSN(dsn, dbName string) (string, error) {
	parsed, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("parse DSN: %w", err)
	}
	parsed.Path = "/" + dbName
	return parsed.String(), nil
}

func applyClientPostureTestMigrations(ctx context.Context, pool *pgxpool.Pool) error {
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

func insertClientPostureWorkspace(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	suffix string,
) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	slug := fmt.Sprintf("client-posture-%s-%d", suffix, time.Now().UnixNano())
	if err := pool.QueryRow(ctx,
		`INSERT INTO workspaces (slug, name, status, trust_domain)
		 VALUES ($1, $1, 'active', $1 || '.test') RETURNING id`,
		slug,
	).Scan(&id); err != nil {
		t.Fatalf("insert workspace: %v", err)
	}
	return id
}

func insertClientPostureUser(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	workspaceID uuid.UUID,
	suffix string,
) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (tenant_id, email, provider, provider_sub, role, status)
		 VALUES ($1, $2, 'test', $3, 'member', 'active') RETURNING id`,
		workspaceID,
		fmt.Sprintf("client-posture-%s@example.test", suffix),
		uuid.NewString(),
	).Scan(&id); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	return id
}

func insertClientPostureDevice(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	workspaceID uuid.UUID,
	userID uuid.UUID,
	suffix string,
) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO client_devices (user_id, workspace_id, name, os)
		 VALUES ($1, $2, $3, 'linux') RETURNING id`,
		userID,
		workspaceID,
		"client-posture-"+suffix,
	).Scan(&id); err != nil {
		t.Fatalf("insert device: %v", err)
	}
	return id
}
