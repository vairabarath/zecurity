package connector

import (
	"context"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	pb "github.com/yourorg/ztna/controller/gen/go/proto/connector/v1"
	"github.com/yourorg/ztna/controller/internal/pki"
	"github.com/yourorg/ztna/controller/internal/spiffe"
)

type recordingPolicyNotifier struct {
	calls int
}

func (r *recordingPolicyNotifier) NotifyPolicyChange(ctx context.Context, workspaceID string) error {
	r.calls++
	return nil
}

type recordingTransportNotifier struct {
	calls []string
}

func (r *recordingTransportNotifier) NotifyTopologyChange(ctx context.Context, workspaceID string, connectorIDs []string) error {
	r.calls = append(r.calls, connectorIDs...)
	return nil
}

type stubControlServer struct {
	ctx context.Context
}

func (s *stubControlServer) SetHeader(metadata.MD) error  { return nil }
func (s *stubControlServer) SendHeader(metadata.MD) error { return nil }
func (s *stubControlServer) SetTrailer(metadata.MD)       {}
func (s *stubControlServer) Context() context.Context     { return s.ctx }
func (s *stubControlServer) SendMsg(m any) error          { return nil }
func (s *stubControlServer) RecvMsg(m any) error          { return io.EOF }
func (s *stubControlServer) Send(*pb.ConnectorControlMessage) error {
	return nil
}
func (s *stubControlServer) Recv() (*pb.ConnectorControlMessage, error) {
	return nil, io.EOF
}

var _ pb.ConnectorService_ControlServer = (*stubControlServer)(nil)
var _ grpc.ServerStream = (*stubControlServer)(nil)

func setupRevocationTestDB(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	dsn := os.Getenv("ENROLLMENT_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ENROLLMENT_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool, ctx
}

func seedRevocationWorkspace(t *testing.T, pool *pgxpool.Pool, ctx context.Context, slug string) (wsID string, trustDomain string, rnID string) {
	t.Helper()
	_, _ = pool.Exec(ctx, `DELETE FROM workspaces WHERE slug = $1`, slug)
	trustDomain = fmt.Sprintf("ws-%s.zecurity.in", slug)
	err := pool.QueryRow(ctx,
		`INSERT INTO workspaces (slug, name, trust_domain, status)
		 VALUES ($1, $1, $2, 'active')
		 RETURNING id`,
		slug, trustDomain,
	).Scan(&wsID)
	if err != nil {
		t.Fatalf("seed workspace (%s): %v", slug, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM workspaces WHERE id = $1`, wsID)
	})

	err = pool.QueryRow(ctx,
		`INSERT INTO remote_networks (tenant_id, name, location)
		 VALUES ($1, 'rn', 'office')
		 RETURNING id`,
		wsID,
	).Scan(&rnID)
	if err != nil {
		t.Fatalf("seed remote_network (%s): %v", slug, err)
	}

	return wsID, trustDomain, rnID
}

func seedConnector(t *testing.T, pool *pgxpool.Pool, ctx context.Context, wsID, rnID, trustDomain, status string, revokedAt *time.Time) string {
	t.Helper()
	var connID string
	err := pool.QueryRow(ctx,
		`INSERT INTO connectors (tenant_id, remote_network_id, name, trust_domain, status, revoked_at)
		 VALUES ($1, $2, 'test-connector', $3, $4, $5)
		 RETURNING id`,
		wsID, rnID, trustDomain, status, revokedAt,
	).Scan(&connID)
	if err != nil {
		t.Fatalf("seed connector: %v", err)
	}
	return connID
}

// 1. TestControl_RejectsRevokedConnector: seed connector status=revoked (revoked_at set too).
func TestControl_RejectsRevokedConnector(t *testing.T) {
	pool, ctx := setupRevocationTestDB(t)
	slug := fmt.Sprintf("rev-ctl-%d", time.Now().UnixNano())
	wsID, trustDomain, rnID := seedRevocationWorkspace(t, pool, ctx, slug)

	now := time.Now()
	connID := seedConnector(t, pool, ctx, wsID, rnID, trustDomain, "revoked", &now)

	h := &EnrollmentHandler{
		Pool:     pool,
		Registry: NewConnectorRegistry(),
	}

	streamCtx := spiffe.WithIdentity(ctx,
		fmt.Sprintf("spiffe://%s/connector/%s", trustDomain, connID),
		"connector",
		connID,
		trustDomain,
	)
	stream := &stubControlServer{ctx: streamCtx}

	err := h.Control(stream)
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected grpc status error, got %v", err)
	}
	if st.Code() != codes.PermissionDenied {
		t.Errorf("got code %v, want %v", st.Code(), codes.PermissionDenied)
	}
	if st.Message() != "connector is revoked" {
		t.Errorf("got message %q, want %q", st.Message(), "connector is revoked")
	}

	// Assert row status remains revoked
	var currentStatus string
	if err := pool.QueryRow(ctx, `SELECT status FROM connectors WHERE id = $1`, connID).Scan(&currentStatus); err != nil {
		t.Fatalf("query connector status: %v", err)
	}
	if currentStatus != "revoked" {
		t.Errorf("connector status became %q, want revoked", currentStatus)
	}
}

// 2. TestControl_RejectsRevokedAtLegacyStatus: seed connector status=disconnected, revoked_at=NOW() (legacy resurrected row).
func TestControl_RejectsRevokedAtLegacyStatus(t *testing.T) {
	pool, ctx := setupRevocationTestDB(t)
	slug := fmt.Sprintf("rev-leg-%d", time.Now().UnixNano())
	wsID, trustDomain, rnID := seedRevocationWorkspace(t, pool, ctx, slug)

	now := time.Now().Truncate(time.Microsecond)
	connID := seedConnector(t, pool, ctx, wsID, rnID, trustDomain, "disconnected", &now)

	h := &EnrollmentHandler{
		Pool:     pool,
		Registry: NewConnectorRegistry(),
	}

	streamCtx := spiffe.WithIdentity(ctx,
		fmt.Sprintf("spiffe://%s/connector/%s", trustDomain, connID),
		"connector",
		connID,
		trustDomain,
	)
	stream := &stubControlServer{ctx: streamCtx}

	err := h.Control(stream)
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected grpc status error, got %v", err)
	}
	if st.Code() != codes.PermissionDenied {
		t.Errorf("got code %v, want %v", st.Code(), codes.PermissionDenied)
	}
	if st.Message() != "connector is revoked" {
		t.Errorf("got message %q, want %q", st.Message(), "connector is revoked")
	}

	// Row stays disconnected, revoked_at unchanged.
	var currentStatus string
	var currentRevokedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT status, revoked_at FROM connectors WHERE id = $1`, connID).Scan(&currentStatus, &currentRevokedAt); err != nil {
		t.Fatalf("query connector status: %v", err)
	}
	if currentStatus != "disconnected" {
		t.Errorf("connector status became %q, want disconnected", currentStatus)
	}
	if currentRevokedAt == nil {
		t.Errorf("connector revoked_at became nil, want non-nil")
	}
}

// 3. TestGoodbye_RevokedStaysRevoked: seed revoked connector; notifier stubs that record calls; call Goodbye via handler with SPIFFE identity injected.
func TestGoodbye_RevokedStaysRevoked(t *testing.T) {
	pool, ctx := setupRevocationTestDB(t)
	slug := fmt.Sprintf("gb-rev-%d", time.Now().UnixNano())
	wsID, trustDomain, rnID := seedRevocationWorkspace(t, pool, ctx, slug)

	now := time.Now()
	connID := seedConnector(t, pool, ctx, wsID, rnID, trustDomain, "revoked", &now)

	policyStub := &recordingPolicyNotifier{}
	transportStub := &recordingTransportNotifier{}

	h := &EnrollmentHandler{
		Pool:              pool,
		PolicyNotifier:    policyStub,
		TransportNotifier: transportStub,
	}

	reqCtx := spiffe.WithIdentity(ctx,
		fmt.Sprintf("spiffe://%s/connector/%s", trustDomain, connID),
		"connector",
		connID,
		trustDomain,
	)

	resp, err := h.Goodbye(reqCtx, &pb.GoodbyeRequest{})
	if err != nil {
		t.Fatalf("Goodbye error: %v", err)
	}
	if resp == nil || !resp.Ok {
		t.Fatalf("Goodbye response = %v, want Ok: true", resp)
	}

	// Status stays revoked
	var currentStatus string
	if err := pool.QueryRow(ctx, `SELECT status FROM connectors WHERE id = $1`, connID).Scan(&currentStatus); err != nil {
		t.Fatalf("query connector status: %v", err)
	}
	if currentStatus != "revoked" {
		t.Errorf("connector status became %q, want revoked", currentStatus)
	}

	// NO notifier calls
	if policyStub.calls != 0 {
		t.Errorf("policyStub.calls = %d, want 0", policyStub.calls)
	}
	if len(transportStub.calls) != 0 {
		t.Errorf("transportStub.calls = %v, want empty", transportStub.calls)
	}
}

// 4. TestGoodbye_ActiveGoesDisconnectedAndNotifies: seed active connector; stubs; EXPECT status becomes disconnected, Ok:true, BOTH PolicyNotifier called and TransportNotifier called.
func TestGoodbye_ActiveGoesDisconnectedAndNotifies(t *testing.T) {
	pool, ctx := setupRevocationTestDB(t)
	slug := fmt.Sprintf("gb-act-%d", time.Now().UnixNano())
	wsID, trustDomain, rnID := seedRevocationWorkspace(t, pool, ctx, slug)

	connID := seedConnector(t, pool, ctx, wsID, rnID, trustDomain, "active", nil)

	policyStub := &recordingPolicyNotifier{}
	transportStub := &recordingTransportNotifier{}

	h := &EnrollmentHandler{
		Pool:              pool,
		PolicyNotifier:    policyStub,
		TransportNotifier: transportStub,
	}

	reqCtx := spiffe.WithIdentity(ctx,
		fmt.Sprintf("spiffe://%s/connector/%s", trustDomain, connID),
		"connector",
		connID,
		trustDomain,
	)

	resp, err := h.Goodbye(reqCtx, &pb.GoodbyeRequest{})
	if err != nil {
		t.Fatalf("Goodbye error: %v", err)
	}
	if resp == nil || !resp.Ok {
		t.Fatalf("Goodbye response = %v, want Ok: true", resp)
	}

	// Status becomes disconnected
	var currentStatus string
	if err := pool.QueryRow(ctx, `SELECT status FROM connectors WHERE id = $1`, connID).Scan(&currentStatus); err != nil {
		t.Fatalf("query connector status: %v", err)
	}
	if currentStatus != "disconnected" {
		t.Errorf("connector status became %q, want disconnected", currentStatus)
	}

	// Both notifiers called
	if policyStub.calls < 1 {
		t.Errorf("policyStub.calls = %d, want >= 1", policyStub.calls)
	}
	found := false
	for _, id := range transportStub.calls {
		if id == connID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("transportStub.calls %v does not contain connectorID %s", transportStub.calls, connID)
	}
}

// ── RenewCert & Health revocation tests ─────────────────────────────────────

// TestRenewCert_RevokedStatusDenied verifies that a connector with status='revoked'
// is rejected before PKIService is called, leaving cert_serial NULL.
func TestRenewCert_RevokedStatusDenied(t *testing.T) {
	pool, ctx := setupRevocationTestDB(t)
	slug := fmt.Sprintf("rc-rev-%d", time.Now().UnixNano())
	wsID, trustDomain, rnID := seedRevocationWorkspace(t, pool, ctx, slug)

	now := time.Now()
	connID := seedConnector(t, pool, ctx, wsID, rnID, trustDomain, "revoked", &now)

	// PKIService is left nil as a canary — any attempt to sign will panic and fail.
	h := &EnrollmentHandler{
		Pool: pool,
	}

	reqCtx := spiffe.WithIdentity(ctx,
		fmt.Sprintf("spiffe://%s/connector/%s", trustDomain, connID),
		"connector",
		connID,
		trustDomain,
	)

	resp, err := h.RenewCert(reqCtx, &pb.RenewCertRequest{PublicKeyDer: []byte("test-key")})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if resp != nil {
		t.Fatalf("expected nil response, got %v", resp)
	}

	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected grpc status error, got %v", err)
	}
	if st.Code() != codes.PermissionDenied {
		t.Errorf("got code %v, want %v", st.Code(), codes.PermissionDenied)
	}
	if st.Message() != "connector is revoked" {
		t.Errorf("got message %q, want %q", st.Message(), "connector is revoked")
	}

	var certSerial *string
	if err := pool.QueryRow(ctx, `SELECT cert_serial FROM connectors WHERE id = $1`, connID).Scan(&certSerial); err != nil {
		t.Fatalf("query cert_serial: %v", err)
	}
	if certSerial != nil {
		t.Errorf("expected cert_serial to stay NULL, got %q", *certSerial)
	}
}

// TestRenewCert_RevokedAtLegacyDenied verifies that a connector with status='disconnected'
// but revoked_at set is rejected before PKIService is called, leaving cert_serial NULL.
func TestRenewCert_RevokedAtLegacyDenied(t *testing.T) {
	pool, ctx := setupRevocationTestDB(t)
	slug := fmt.Sprintf("rc-leg-%d", time.Now().UnixNano())
	wsID, trustDomain, rnID := seedRevocationWorkspace(t, pool, ctx, slug)

	now := time.Now().Truncate(time.Microsecond)
	connID := seedConnector(t, pool, ctx, wsID, rnID, trustDomain, "disconnected", &now)

	// PKIService is left nil as a canary.
	h := &EnrollmentHandler{
		Pool: pool,
	}

	reqCtx := spiffe.WithIdentity(ctx,
		fmt.Sprintf("spiffe://%s/connector/%s", trustDomain, connID),
		"connector",
		connID,
		trustDomain,
	)

	resp, err := h.RenewCert(reqCtx, &pb.RenewCertRequest{PublicKeyDer: []byte("test-key")})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if resp != nil {
		t.Fatalf("expected nil response, got %v", resp)
	}

	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected grpc status error, got %v", err)
	}
	if st.Code() != codes.PermissionDenied {
		t.Errorf("got code %v, want %v", st.Code(), codes.PermissionDenied)
	}
	if st.Message() != "connector is revoked" {
		t.Errorf("got message %q, want %q", st.Message(), "connector is revoked")
	}

	var certSerial *string
	if err := pool.QueryRow(ctx, `SELECT cert_serial FROM connectors WHERE id = $1`, connID).Scan(&certSerial); err != nil {
		t.Fatalf("query cert_serial: %v", err)
	}
	if certSerial != nil {
		t.Errorf("expected cert_serial to stay NULL, got %q", *certSerial)
	}
}

type raceRevokePKIStub struct {
	pki.Service
	pool        *pgxpool.Pool
	connectorID string
}

func (s *raceRevokePKIStub) RenewConnectorCert(ctx context.Context, tenantID, connectorID, trustDomain string, publicKeyDER []byte, certTTL time.Duration) (*pki.ConnectorCertResult, error) {
	// Side-effect: simulate concurrent revocation
	_, err := s.pool.Exec(ctx, `UPDATE connectors SET status = 'revoked', revoked_at = NOW() WHERE id = $1`, s.connectorID)
	if err != nil {
		return nil, fmt.Errorf("simulate revoke race: %w", err)
	}
	return &pki.ConnectorCertResult{
		CertificatePEM: "dummy-cert",
		Serial:         "dummy-serial-12345",
		NotBefore:      time.Now(),
		NotAfter:       time.Now().Add(time.Hour),
	}, nil
}

// TestRenewCert_RaceRevokeReturnsNoCert verifies that if a connector is revoked
// concurrently during renewal, the guarded UPDATE affects 0 rows, returning
// PermissionDenied with no cert persisted.
func TestRenewCert_RaceRevokeReturnsNoCert(t *testing.T) {
	pool, ctx := setupRevocationTestDB(t)
	slug := fmt.Sprintf("rc-race-%d", time.Now().UnixNano())
	wsID, trustDomain, rnID := seedRevocationWorkspace(t, pool, ctx, slug)

	connID := seedConnector(t, pool, ctx, wsID, rnID, trustDomain, "active", nil)

	pkiStub := &raceRevokePKIStub{
		pool:        pool,
		connectorID: connID,
	}

	h := &EnrollmentHandler{
		Pool:       pool,
		PKIService: pkiStub,
		Cfg: Config{
			CertTTL: time.Hour,
		},
	}

	reqCtx := spiffe.WithIdentity(ctx,
		fmt.Sprintf("spiffe://%s/connector/%s", trustDomain, connID),
		"connector",
		connID,
		trustDomain,
	)

	resp, err := h.RenewCert(reqCtx, &pb.RenewCertRequest{PublicKeyDer: []byte("test-key")})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if resp != nil {
		t.Fatalf("expected nil response, got %v", resp)
	}

	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected grpc status error, got %v", err)
	}
	if st.Code() != codes.PermissionDenied {
		t.Errorf("got code %v, want %v", st.Code(), codes.PermissionDenied)
	}
	if st.Message() != "connector is revoked" {
		t.Errorf("got message %q, want %q", st.Message(), "connector is revoked")
	}

	var certSerial *string
	if err := pool.QueryRow(ctx, `SELECT cert_serial FROM connectors WHERE id = $1`, connID).Scan(&certSerial); err != nil {
		t.Fatalf("query cert_serial: %v", err)
	}
	if certSerial != nil {
		t.Errorf("expected cert_serial to stay NULL, got %q", *certSerial)
	}
}

// TestHandleConnectorHealth_RevokedClosesStream verifies that when a connector's
// row is revoked, handleConnectorHealth returns false to signal stream teardown.
func TestHandleConnectorHealth_RevokedClosesStream(t *testing.T) {
	pool, ctx := setupRevocationTestDB(t)
	slug := fmt.Sprintf("hc-rev-%d", time.Now().UnixNano())
	wsID, trustDomain, rnID := seedRevocationWorkspace(t, pool, ctx, slug)

	connID := seedConnector(t, pool, ctx, wsID, rnID, trustDomain, "active", nil)

	// Revoke in DB
	if _, err := pool.Exec(ctx, `UPDATE connectors SET status = 'revoked', revoked_at = NOW() WHERE id = $1`, connID); err != nil {
		t.Fatalf("revoke connector: %v", err)
	}

	h := &EnrollmentHandler{
		Pool: pool,
	}

	client := testClient(connID, wsID)
	ok := h.handleConnectorHealth(ctx, client, &pb.ConnectorHealthReport{
		Version:    "test",
		Hostname:   "h",
		AclVersion: 0,
	})
	if ok {
		t.Fatal("expected handleConnectorHealth to return false for revoked connector, got true")
	}

	// Assert row is still status=revoked and revoked_at is unchanged (non-nil)
	var status string
	var revokedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT status, revoked_at FROM connectors WHERE id = $1`, connID).Scan(&status, &revokedAt); err != nil {
		t.Fatalf("query connector status: %v", err)
	}
	if status != "revoked" {
		t.Errorf("expected status 'revoked', got %q", status)
	}
	if revokedAt == nil {
		t.Error("expected revoked_at to remain non-nil")
	}
}
