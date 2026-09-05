package client

import (
	"context"
	"crypto/x509"
	"encoding/json"
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

	"github.com/yourorg/ztna/controller/internal/identity"
	"github.com/yourorg/ztna/controller/internal/outbox"
	"github.com/yourorg/ztna/controller/internal/pki"
)

func devTrustTestPool(t *testing.T, ctx context.Context, dsn string) *pgxpool.Pool {
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

func devTrustTestDBName(t *testing.T) string {
	name := strings.ToLower(t.Name())
	name = strings.ReplaceAll(name, "/", "_")
	if len(name) > 32 {
		name = name[:32]
	}
	return fmt.Sprintf("%s_%d_%d", name, os.Getpid(), time.Now().UnixNano())
}

func devTrustTestDSN(dsn, dbName string) (string, error) {
	parsed, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("parse DSN: %w", err)
	}
	parsed.Path = "/" + dbName
	return parsed.String(), nil
}

func devTrustApplyMigrations(ctx context.Context, pool *pgxpool.Pool) error {
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

// insertDevTrustWorkspace creates a workspace with a workspace CA so that
// GenerateClientCRL can decrypt and sign. Returns the workspace UUID string.
// Note: GenerateWorkspaceCA only returns the key; the bootstrap layer normally
// persists it to workspace_ca_keys — we replicate that here so the CRL can load
// the CA key.
func insertDevTrustWorkspace(t *testing.T, ctx context.Context, pool *pgxpool.Pool, pkiSvc pki.Service, suffix string) string {
	t.Helper()
	var id string
	slug := fmt.Sprintf("dev-trust-%s-%d", suffix, time.Now().UnixNano())
	if err := pool.QueryRow(ctx,
		`INSERT INTO workspaces (slug, name, status, trust_domain)
		 VALUES ($1, $1, 'active', $1 || '.test') RETURNING id`,
		slug,
	).Scan(&id); err != nil {
		t.Fatalf("insert workspace: %v", err)
	}
	caResult, err := pkiSvc.GenerateWorkspaceCA(ctx, id)
	if err != nil {
		t.Fatalf("generate workspace CA: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO workspace_ca_keys
		   (tenant_id, encrypted_private_key, nonce, key_algorithm, certificate_pem, not_before, not_after)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		id, caResult.EncryptedPrivateKey, caResult.Nonce, caResult.KeyAlgorithm,
		caResult.CertificatePEM, caResult.NotBefore, caResult.NotAfter,
	); err != nil {
		t.Fatalf("insert workspace_ca_keys: %v", err)
	}
	return id
}

func insertDevTrustUser(t *testing.T, ctx context.Context, pool *pgxpool.Pool, workspaceID, suffix string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (tenant_id, email, provider, provider_sub, role, status)
		 VALUES ($1, $2, 'test', $3, 'member', 'active') RETURNING id`,
		workspaceID,
		fmt.Sprintf("dev-trust-%s@example.test", suffix),
		uuid.NewString(),
	).Scan(&id); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	return id
}

// insertDevTrustDevice inserts a client_device. serial may be "" to exercise the
// CRL's `cert_serial IS NOT NULL` guard (devices without a serial are never put
// on the CRL).
func insertDevTrustDevice(t *testing.T, ctx context.Context, pool *pgxpool.Pool, workspaceID, userID, name, serial string) string {
	t.Helper()
	var id string
	var err error
	if serial == "" {
		err = pool.QueryRow(ctx,
			`INSERT INTO client_devices (user_id, workspace_id, name, os)
			 VALUES ($1, $2, $3, 'linux') RETURNING id`,
			userID, workspaceID, name,
		).Scan(&id)
	} else {
		err = pool.QueryRow(ctx,
			`INSERT INTO client_devices (user_id, workspace_id, name, os, cert_serial, cert_not_after)
			 VALUES ($1, $2, $3, 'linux', $4, NOW() + INTERVAL '6 days') RETURNING id`,
			userID, workspaceID, name, serial,
		).Scan(&id)
	}
	if err != nil {
		t.Fatalf("insert device %s: %v", name, err)
	}
	return id
}

func newDevTrustEvent(eventType, workspaceID, userID, correlationID string, reason identity.DeviceTrustReason) outbox.OutboxEvent {
	wID, _ := uuid.Parse(workspaceID)
	uID, _ := uuid.Parse(userID)
	cID, _ := uuid.Parse(correlationID)
	payload, _ := json.Marshal(identity.DeviceTrustEvent{
		WorkspaceID:   workspaceID,
		UserID:        userID,
		Reason:        reason,
		CorrelationID: correlationID,
	})
	return outbox.OutboxEvent{
		EventType:     eventType,
		WorkspaceID:   wID,
		UserID:        &uID,
		CorrelationID: cID,
		Payload:       payload,
	}
}

func revokedSerials(t *testing.T, ctx context.Context, pkiSvc pki.Service, workspaceID string) []string {
	t.Helper()
	der, err := pkiSvc.GenerateClientCRL(ctx, workspaceID)
	if err != nil {
		t.Fatalf("GenerateClientCRL: %v", err)
	}
	list, err := parseCRLSerials(der)
	if err != nil {
		t.Fatalf("parse CRL: %v", err)
	}
	return list
}

// TestDeviceTrustRevokeRequested covers the core Track 1 loop: a
// device.trust.revoke.requested event revokes all of a user's devices (scoped
// to workspace+user), reaches the workspace CRL, and writes a single audit row
// with source:scim + correlation_id. A second user's device is untouched
// (tenant isolation), and a device without a cert_serial never lands on the
// CRL.
func TestDeviceTrustRevokeRequested(t *testing.T) {
	adminDSN := os.Getenv("PKI_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("PKI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	dbName := devTrustTestDBName(t)
	adminPool := devTrustTestPool(t, ctx, adminDSN)
	defer adminPool.Close()
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	defer func() {
		if _, err := adminPool.Exec(ctx, "DROP DATABASE IF EXISTS "+dbName); err != nil {
			t.Logf("drop test database: %v", err)
		}
	}()
	testDSN, err := devTrustTestDSN(adminDSN, dbName)
	if err != nil {
		t.Fatalf("build test database DSN: %v", err)
	}
	pool := devTrustTestPool(t, ctx, testDSN)
	defer pool.Close()
	if err := devTrustApplyMigrations(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	t.Setenv("PKI_MASTER_SECRET", "dev-trust-handler-integration-secret")
	pkiSvc, err := pki.Init(ctx, pool)
	if err != nil {
		t.Fatalf("pki.Init: %v", err)
	}

	wsID := insertDevTrustWorkspace(t, ctx, pool, pkiSvc, "ws")
	user1 := insertDevTrustUser(t, ctx, pool, wsID, "u1")
	user2 := insertDevTrustUser(t, ctx, pool, wsID, "u2")

	// user1: 2 devices with serials + 1 without.
	d1 := insertDevTrustDevice(t, ctx, pool, wsID, user1, "u1-laptop", "AABBCCDDEEFF0011")
	d2 := insertDevTrustDevice(t, ctx, pool, wsID, user1, "u1-phone", "AABBCCDDEEFF0022")
	insertDevTrustDevice(t, ctx, pool, wsID, user1, "u1-no-serial", "")
	// user2: untouched by the event.
	d3 := insertDevTrustDevice(t, ctx, pool, wsID, user2, "u2-laptop", "AABBCCDDEEFF0033")

	handler := NewDeviceTrustRevokeHandler(pool, nil)
	evt := newDevTrustEvent(
		identity.EventDeviceTrustRevokeRequested,
		wsID, user1, uuid.NewString(), identity.DeviceTrustReasonSuspended,
	)

	if err := handler.Handle(ctx, evt); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// user1's serial'd devices revoked; the no-serial device also gets
	// revoked_at (it just won't appear on the CRL).
	assertRevoked(t, ctx, pool, d1, true)
	assertRevoked(t, ctx, pool, d2, true)
	// u1-no-serial also gets revoked_at from the all-of-user UPDATE (it just
	// won't appear on the CRL for lack of a cert_serial). Check by name.
	var noSerialRevoked bool
	if err := pool.QueryRow(ctx,
		`SELECT revoked_at IS NOT NULL FROM client_devices WHERE user_id = $1 AND name = 'u1-no-serial'`,
		user1,
	).Scan(&noSerialRevoked); err != nil {
		t.Fatalf("check no-serial revoked: %v", err)
	}
	if !noSerialRevoked {
		t.Fatal("u1-no-serial should have been revoked by the all-of-user UPDATE")
	}
	assertRevoked(t, ctx, pool, d3, false) // user2 untouched

	// Tenant isolation: exactly the 3 user1 devices revoked, not user2's.
	var revokedCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM client_devices WHERE workspace_id = $1 AND user_id = $2 AND revoked_at IS NOT NULL`,
		wsID, user1,
	).Scan(&revokedCount); err != nil {
		t.Fatalf("count revoked: %v", err)
	}
	if revokedCount != 3 {
		t.Fatalf("expected 3 revoked user1 devices, got %d", revokedCount)
	}

	// The serial'd revocations must reach the CRL.
	serials := revokedSerials(t, ctx, pkiSvc, wsID)
	for _, want := range []string{"AABBCCDDEEFF0011", "AABBCCDDEEFF0022"} {
		if !containsSerial(serials, want) {
			t.Fatalf("expected revoked serial %s on CRL, got %v", want, serials)
		}
	}
	if containsSerial(serials, "AABBCCDDEEFF0033") {
		t.Fatalf("user2 serial %s must NOT be on the CRL (tenant isolation)", "AABBCCDDEEFF0033")
	}

	// Single audit row with source:scim + correlation_id.
	assertAuditRow(t, ctx, pool, wsID, "device.revoked", user1, evt.CorrelationID.String())
}

// TestDeviceTrustRevokeIdempotent ensures replay is a no-op: a second Handle on
// an already-revoked user affects 0 rows, writes no second audit row, and
// returns nil.
func TestDeviceTrustRevokeIdempotent(t *testing.T) {
	adminDSN := os.Getenv("PKI_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("PKI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	dbName := devTrustTestDBName(t)
	adminPool := devTrustTestPool(t, ctx, adminDSN)
	defer adminPool.Close()
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	defer func() {
		if _, err := adminPool.Exec(ctx, "DROP DATABASE IF EXISTS "+dbName); err != nil {
			t.Logf("drop test database: %v", err)
		}
	}()
	testDSN, err := devTrustTestDSN(adminDSN, dbName)
	if err != nil {
		t.Fatalf("build test database DSN: %v", err)
	}
	pool := devTrustTestPool(t, ctx, testDSN)
	defer pool.Close()
	if err := devTrustApplyMigrations(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	t.Setenv("PKI_MASTER_SECRET", "dev-trust-handler-integration-secret")
	pkiSvc, err := pki.Init(ctx, pool)
	if err != nil {
		t.Fatalf("pki.Init: %v", err)
	}
	wsID := insertDevTrustWorkspace(t, ctx, pool, pkiSvc, "ws")
	user1 := insertDevTrustUser(t, ctx, pool, wsID, "u1")
	insertDevTrustDevice(t, ctx, pool, wsID, user1, "u1-laptop", "AABBCCDDEEFF0011")

	handler := NewDeviceTrustRevokeHandler(pool, nil)
	corr := uuid.NewString()
	evt := newDevTrustEvent(identity.EventDeviceTrustRevokeRequested, wsID, user1, corr, identity.DeviceTrustReasonDeleted)

	if err := handler.Handle(ctx, evt); err != nil {
		t.Fatalf("first Handle: %v", err)
	}
	if err := handler.Handle(ctx, evt); err != nil { // replay
		t.Fatalf("replay Handle: %v", err)
	}

	var auditCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_logs WHERE tenant_id = $1 AND action = 'device.revoked' AND target_id = $2`,
		wsID, user1,
	).Scan(&auditCount); err != nil {
		t.Fatalf("count audit: %v", err)
	}
	if auditCount != 1 {
		t.Fatalf("expected exactly 1 audit row across replay, got %d", auditCount)
	}
}

// TestDeviceTrustRevokeMalformed ensures a missing typed UserID column surfaces
// as an error (a failed event), rather than silently doing nothing.
func TestDeviceTrustRevokeMalformed(t *testing.T) {
	adminDSN := os.Getenv("PKI_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("PKI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool := devTrustTestPool(t, ctx, adminDSN) // admin DSN is a real DB; we only need a pool, no writes
	defer pool.Close()

	handler := NewDeviceTrustRevokeHandler(pool, nil)

	wID, _ := uuid.Parse("11111111-1111-1111-1111-111111111111")
	cID := uuid.New()
	evt := outbox.OutboxEvent{
		EventType:     identity.EventDeviceTrustRevokeRequested,
		WorkspaceID:   wID,
		UserID:        nil, // malformed
		CorrelationID: cID,
	}
	if err := handler.Handle(ctx, evt); err == nil {
		t.Fatal("expected error for missing typed UserID, got nil")
	}
}

// TestDeviceTrustReEnrollRequired ensures the reactivation event sets
// status='re_enroll_required' on the user's devices (Sprint 19 Track 2),
// without un-revoking them, and records an audit row.
func TestDeviceTrustReEnrollRequired(t *testing.T) {
	adminDSN := os.Getenv("PKI_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("PKI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	dbName := devTrustTestDBName(t)
	adminPool := devTrustTestPool(t, ctx, adminDSN)
	defer adminPool.Close()
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	defer func() {
		if _, err := adminPool.Exec(ctx, "DROP DATABASE IF EXISTS "+dbName); err != nil {
			t.Logf("drop test database: %v", err)
		}
	}()
	testDSN, err := devTrustTestDSN(adminDSN, dbName)
	if err != nil {
		t.Fatalf("build test database DSN: %v", err)
	}
	pool := devTrustTestPool(t, ctx, testDSN)
	defer pool.Close()
	if err := devTrustApplyMigrations(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	t.Setenv("PKI_MASTER_SECRET", "dev-trust-handler-integration-secret")
	pkiSvc, err := pki.Init(ctx, pool)
	if err != nil {
		t.Fatalf("pki.Init: %v", err)
	}
	wsID := insertDevTrustWorkspace(t, ctx, pool, pkiSvc, "ws")
	user1 := insertDevTrustUser(t, ctx, pool, wsID, "u1")
	// Pre-revoked device (simulating the prior suspend revoked it).
	insertDevTrustDevice(t, ctx, pool, wsID, user1, "u1-laptop", "AABBCCDDEEFF0011")
	if _, err := pool.Exec(ctx,
		`UPDATE client_devices SET revoked_at = NOW() WHERE user_id = $1`, user1); err != nil {
		t.Fatalf("pre-revoke: %v", err)
	}

	handler := NewDeviceTrustReEnrollHandler(pool)
	corr := uuid.NewString()
	evt := newDevTrustEvent(identity.EventDeviceTrustReEnrollmentRequired, wsID, user1, corr, identity.DeviceTrustReason(""))

	if err := handler.Handle(ctx, evt); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// Still revoked — the handler must not un-revoke (revoked_at is terminal).
	var stillRevoked int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM client_devices WHERE user_id = $1 AND revoked_at IS NOT NULL`,
		user1,
	).Scan(&stillRevoked); err != nil {
		t.Fatalf("count: %v", err)
	}
	if stillRevoked != 1 {
		t.Fatalf("re-enroll handler must not un-revoke devices; revoked count = %d", stillRevoked)
	}

	// But status is now re_enroll_required, so deviceGate reports the
	// recoverable directive instead of the terminal REVOKED one.
	var reEnrollRequired int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM client_devices WHERE user_id = $1 AND status = 're_enroll_required'`,
		user1,
	).Scan(&reEnrollRequired); err != nil {
		t.Fatalf("count: %v", err)
	}
	if reEnrollRequired != 1 {
		t.Fatalf("re-enroll handler must set status = 're_enroll_required'; count = %d", reEnrollRequired)
	}

	assertAuditRow(t, ctx, pool, wsID, "device.re_enroll_required", user1, corr)
}

// ── helpers ────────────────────────────────────────────────────────────────

func assertRevoked(t *testing.T, ctx context.Context, pool *pgxpool.Pool, deviceID string, want bool) {
	t.Helper()
	var got bool
	if err := pool.QueryRow(ctx,
		`SELECT revoked_at IS NOT NULL FROM client_devices WHERE id = $1`, deviceID,
	).Scan(&got); err != nil {
		t.Fatalf("check revoked %s: %v", deviceID, err)
	}
	if got != want {
		t.Fatalf("device %s: revoked_at set = %v, want %v", deviceID, got, want)
	}
}

func assertAuditRow(t *testing.T, ctx context.Context, pool *pgxpool.Pool, wsID, action, targetID, corr string) {
	t.Helper()
	var count int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_logs
		  WHERE tenant_id = $1 AND action = $2 AND target_id = $3
		    AND details::jsonb ->> 'correlation_id' = $4`,
		wsID, action, targetID, corr,
	).Scan(&count); err != nil {
		t.Fatalf("count audit: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 audit row (action=%s, target=%s, corr=%s), got %d", action, targetID, corr, count)
	}
}

// parseCRLSerials decodes a DER-encoded CRL and returns its revoked serials as
// uppercase hex strings (matching the cert_serial storage format).
func parseCRLSerials(der []byte) ([]string, error) {
	list, err := x509.ParseRevocationList(der)
	if err != nil {
		return nil, fmt.Errorf("parse revocation list: %w", err)
	}
	out := make([]string, 0, len(list.RevokedCertificates))
	for _, rc := range list.RevokedCertificates {
		out = append(out, strings.ToUpper(rc.SerialNumber.Text(16)))
	}
	return out, nil
}

func containsSerial(serials []string, want string) bool {
	for _, s := range serials {
		if s == strings.ToUpper(want) {
			return true
		}
	}
	return false
}
