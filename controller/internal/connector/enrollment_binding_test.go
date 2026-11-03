package connector

import (
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/valkey-io/valkey-go/valkeycompat"
	pb "github.com/yourorg/ztna/controller/gen/go/proto/connector/v1"
	"github.com/yourorg/ztna/controller/internal/appmeta"
	"github.com/yourorg/ztna/controller/internal/pki"
	"github.com/yourorg/ztna/controller/internal/tenant"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Certificate audit bug 8: re-issuing a connector enrollment token did not
// cancel the previous one, and Enroll never checked that the presented token
// was the connector's current one, so every earlier token stayed usable until
// it expired. Two valid tokens used at once could both be signed, while only
// the last writer's serial was recorded: the other certificate worked but
// could never be revoked (CRLs list recorded serials only).

type bindingFixture struct {
	t      *testing.T
	pool   *pgxpool.Pool
	ctx    context.Context
	pki    pki.Service
	cfg    Config
	wsID   string
	td     string
	connID string
	// key and leaf: the connector's key and its current certificate, once
	// enrolled with enrollWithKey (renewal must present both).
	key  *ecdsa.PrivateKey
	leaf *x509.Certificate
}

func newBindingFixture(t *testing.T) *bindingFixture {
	t.Helper()
	pool, ctx := setupRevocationTestDB(t)
	t.Setenv("PKI_MASTER_SECRET", "enrollment-binding-test-secret")
	pkiSvc, err := pki.Init(ctx, pool)
	if err != nil {
		t.Fatalf("pki.Init: %v", err)
	}
	slug := fmt.Sprintf("bind-%d", time.Now().UnixNano())
	f := &bindingFixture{
		t: t, pool: pool, ctx: ctx, pki: pkiSvc,
		cfg: Config{JWTSecret: "binding-jwt-secret", CertTTL: time.Hour, EnrollmentTokenTTL: 5 * time.Minute},
		td:  appmeta.WorkspaceTrustDomain(slug),
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO workspaces (slug, name, status, trust_domain) VALUES ($1, $1, 'active', $2) RETURNING id::text`,
		slug, f.td).Scan(&f.wsID); err != nil {
		t.Fatalf("insert workspace: %v", err)
	}
	ca, err := pkiSvc.GenerateWorkspaceCA(ctx, f.wsID)
	if err != nil {
		t.Fatalf("GenerateWorkspaceCA: %v", err)
	}
	f.exec(`INSERT INTO workspace_ca_keys
	          (tenant_id, encrypted_private_key, nonce, certificate_pem, key_algorithm, not_before, not_after)
	        VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		f.wsID, ca.EncryptedPrivateKey, ca.Nonce, ca.CertificatePEM, ca.KeyAlgorithm, ca.NotBefore, ca.NotAfter)
	rnID := insertEnrollmentRemoteNetwork(t, ctx, pool, f.wsID)
	if err := pool.QueryRow(ctx,
		`INSERT INTO connectors (tenant_id, remote_network_id, name, status)
		 VALUES ($1, $2, 'binding-connector', 'pending') RETURNING id::text`,
		f.wsID, rnID).Scan(&f.connID); err != nil {
		t.Fatalf("insert connector: %v", err)
	}
	return f
}

func (f *bindingFixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx, sql, args...); err != nil {
		f.t.Fatalf("exec %q: %v", sql, err)
	}
}

func (f *bindingFixture) handler(pkiSvc pki.Service) *EnrollmentHandler {
	return &EnrollmentHandler{Cfg: f.cfg, Pool: f.pool, Redis: testValkeyRDB, PKIService: pkiSvc}
}

var enrollmentTokenRE = regexp.MustCompile(`ENROLLMENT_TOKEN=(\S+)`)

// reissue calls the real token endpoint, as the connector detail page does.
// Returns the token ("" when refused) and the HTTP status.
func (f *bindingFixture) reissue(rdb valkeycompat.Cmdable) (string, int) {
	req := httptest.NewRequest(http.MethodPost, "/api/connectors/"+f.connID+"/token", nil)
	req = req.WithContext(tenant.Set(f.ctx, tenant.TenantContext{TenantID: f.wsID}))
	rec := httptest.NewRecorder()
	RegenerateTokenHandler(f.pool, f.cfg, rdb).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		return "", rec.Code
	}
	// The body is a single-use enrollment token (live run 2026-10-29, O-1).
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		f.t.Errorf("token response Cache-Control = %q, want no-store", cc)
	}
	var resp regenerateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		return "", -1
	}
	m := enrollmentTokenRE.FindStringSubmatch(resp.InstallCommand)
	if m == nil {
		return "", -1
	}
	return m[1], rec.Code
}

func (f *bindingFixture) mustReissue() string {
	f.t.Helper()
	token, code := f.reissue(testValkeyRDB)
	if code != http.StatusOK || token == "" {
		f.t.Fatalf("re-issue: status %d", code)
	}
	return token
}

func (f *bindingFixture) jti(token string) string {
	f.t.Helper()
	claims, err := VerifyEnrollmentToken(f.cfg, token)
	if err != nil {
		f.t.Fatalf("verify token: %v", err)
	}
	return claims.ID
}

func (f *bindingFixture) request(token string) *pb.EnrollRequest {
	return &pb.EnrollRequest{
		EnrollmentToken: token,
		CsrDer:          generateConnectorCSR(f.t, f.connID, f.td),
		Version:         "1.0.0",
		Hostname:        "binding-host",
	}
}

type connectorRow struct {
	status, serial, jti string
}

func (f *bindingFixture) row() connectorRow {
	f.t.Helper()
	var r connectorRow
	if err := f.pool.QueryRow(f.ctx,
		`SELECT status, COALESCE(cert_serial, ''), COALESCE(enrollment_token_jti, '') FROM connectors WHERE id = $1`,
		f.connID).Scan(&r.status, &r.serial, &r.jti); err != nil {
		f.t.Fatalf("read connector: %v", err)
	}
	return r
}

func certSerial(t *testing.T, resp *pb.EnrollResponse) string {
	t.Helper()
	block, _ := pem.Decode(resp.CertificatePem)
	if block == nil {
		t.Fatal("certificate PEM does not decode")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	return cert.SerialNumber.Text(16)
}

// enrolledAs asserts a successful enrollment whose certificate serial is the
// one durably recorded on an active connector whose token is consumed.
func (f *bindingFixture) enrolledAs(resp *pb.EnrollResponse, err error) {
	f.t.Helper()
	if err != nil {
		f.t.Fatalf("enroll: %v", err)
	}
	r := f.row()
	if r.status != "active" || r.jti != "" {
		f.t.Fatalf("row after enrollment = %+v, want active with the token consumed", r)
	}
	if got := certSerial(f.t, resp); got != r.serial {
		f.t.Fatalf("returned certificate serial %s is not the recorded serial %s", got, r.serial)
	}
}

func refusedEnrollment(t *testing.T, resp *pb.EnrollResponse, err error, code codes.Code) {
	t.Helper()
	if got := status.Code(err); got != code {
		t.Fatalf("code = %v (err %v), want %v", got, err, code)
	}
	if resp != nil {
		t.Fatal("a certificate was returned")
	}
}

// signHookPKI runs during once, inside the first signing call, before the
// certificate is returned to the handler.
type signHookPKI struct {
	pki.Service
	once   sync.Once
	during func()
	mu     sync.Mutex
	calls  int
}

func (p *signHookPKI) SignConnectorCert(ctx context.Context, tenantID, connectorID, trustDomain string, csr *x509.CertificateRequest, ttl time.Duration) (*pki.ConnectorCertResult, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	if p.during != nil {
		p.once.Do(p.during)
	}
	return p.Service.SignConnectorCert(ctx, tenantID, connectorID, trustDomain, csr, ttl)
}

// runUntilBlockedOrDone starts fn in a goroutine and returns once fn has
// finished or is waiting on a database row lock. Polls; no fixed sleeps.
func runUntilBlockedOrDone(t *testing.T, pool *pgxpool.Pool, fn func()) <-chan struct{} {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); fn() }()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-done:
			return done
		default:
		}
		var waiting int
		if err := pool.QueryRow(context.Background(),
			`SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`,
		).Scan(&waiting); err == nil && waiting > 0 {
			return done
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("concurrent operation neither finished nor blocked on a row lock")
	return done
}

func TestEnrollBinding_NormalEnrollment(t *testing.T) {
	f := newBindingFixture(t)
	token := f.mustReissue()
	if got := f.row().jti; got != f.jti(token) {
		t.Fatalf("row token %q, want the minted token %q", got, f.jti(token))
	}
	resp, err := f.handler(f.pki).Enroll(f.ctx, f.request(token))
	f.enrolledAs(resp, err)
	if _, found, _ := PeekEnrollmentJTIForTest(f.ctx, testValkeyRDB, f.jti(token)); found {
		t.Fatal("the token's Valkey entry survived a successful enrollment")
	}

	// Retrying the winning token is refused.
	resp, err = f.handler(f.pki).Enroll(f.ctx, f.request(token))
	refusedEnrollment(t, resp, err, codes.PermissionDenied)
}

// Issuing a token is audited (O-2) without the token.
func TestEnrollBinding_TokenIssueAuditedWithoutToken(t *testing.T) {
	f := newBindingFixture(t)
	token := f.mustReissue()
	var n int
	var details string
	if err := f.pool.QueryRow(f.ctx,
		`SELECT count(*), coalesce(string_agg(coalesce(details::text, ''), ''), '') FROM audit_logs
		  WHERE tenant_id = $1 AND action = 'connector.token.issue' AND target_id = $2`,
		f.wsID, f.connID).Scan(&n, &details); err != nil {
		t.Fatalf("audit query: %v", err)
	}
	if n != 1 {
		t.Fatalf("connector.token.issue rows = %d, want 1", n)
	}
	if strings.Contains(details, token) || strings.Contains(details, f.jti(token)) {
		t.Fatalf("audit details carry the token: %s", details)
	}
}

func TestEnrollBinding_SupersededTokenRejected(t *testing.T) {
	f := newBindingFixture(t)
	tokenA := f.mustReissue()
	tokenB := f.mustReissue()
	// Even if A's Valkey entry is still present, A is no longer current.
	if err := StoreEnrollmentJTI(f.ctx, testValkeyRDB, f.jti(tokenA), f.connID, time.Minute); err != nil {
		t.Fatalf("restore token A entry: %v", err)
	}
	resp, err := f.handler(f.pki).Enroll(f.ctx, f.request(tokenA))
	refusedEnrollment(t, resp, err, codes.PermissionDenied)
	if r := f.row(); r.status != "pending" || r.jti != f.jti(tokenB) {
		t.Fatalf("row after refused superseded token = %+v", r)
	}

	resp, err = f.handler(f.pki).Enroll(f.ctx, f.request(tokenB))
	f.enrolledAs(resp, err)
}

func TestEnrollBinding_ReissueDeletesPredecessorEntry(t *testing.T) {
	f := newBindingFixture(t)
	tokenA := f.mustReissue()
	tokenB := f.mustReissue()
	if _, found, _ := PeekEnrollmentJTIForTest(f.ctx, testValkeyRDB, f.jti(tokenA)); found {
		t.Fatal("re-issue left the predecessor's Valkey entry behind")
	}
	if _, found, _ := PeekEnrollmentJTIForTest(f.ctx, testValkeyRDB, f.jti(tokenB)); !found {
		t.Fatal("re-issue did not store the new token's entry")
	}
}

// Valkey cleanup of the predecessor fails: the old token still cannot enroll,
// because the row's token ID moved on.
func TestEnrollBinding_PredecessorCleanupFailureStillRejectsOldToken(t *testing.T) {
	f := newBindingFixture(t)
	tokenA := f.mustReissue()
	tokenB, code := f.reissue(failingDel{testValkeyRDB})
	if code != http.StatusOK || tokenB == "" {
		t.Fatalf("re-issue with failing cleanup: status %d (cleanup is best effort)", code)
	}
	if _, found, _ := PeekEnrollmentJTIForTest(f.ctx, testValkeyRDB, f.jti(tokenA)); !found {
		t.Fatal("test setup: token A's entry should have survived the failed cleanup")
	}
	resp, err := f.handler(f.pki).Enroll(f.ctx, f.request(tokenA))
	refusedEnrollment(t, resp, err, codes.PermissionDenied)

	resp, err = f.handler(f.pki).Enroll(f.ctx, f.request(tokenB))
	f.enrolledAs(resp, err)
}

// failingDel fails every DEL.
type failingDel struct{ valkeycompat.Cmdable }

func (failingDel) Del(context.Context, ...string) *valkeycompat.IntCmd {
	cmd := &valkeycompat.IntCmd{}
	cmd.SetErr(errors.New("valkey unavailable"))
	return cmd
}

// The same token, twice at once: exactly one enrollment, and its certificate
// is the recorded one.
func TestEnrollBinding_ConcurrentSameToken(t *testing.T) {
	f := newBindingFixture(t)
	token := f.mustReissue()
	first, second := f.request(token), f.request(token)
	var secondResp *pb.EnrollResponse
	var secondErr error
	var done <-chan struct{}
	hook := &signHookPKI{Service: f.pki}
	hook.during = func() {
		done = runUntilBlockedOrDone(t, f.pool, func() {
			secondResp, secondErr = f.handler(f.pki).Enroll(f.ctx, second)
		})
	}
	resp, err := f.handler(hook).Enroll(f.ctx, first)
	<-done
	f.enrolledAs(resp, err)
	refusedEnrollment(t, secondResp, secondErr, codes.PermissionDenied)
}

// A superseded token (its Valkey entry still present) and the current one, at
// once: never two certificates, and the returned one is recorded.
func TestEnrollBinding_ConcurrentSupersededAndCurrentTokens(t *testing.T) {
	f := newBindingFixture(t)
	tokenA := f.mustReissue()
	tokenB := f.mustReissue()
	if err := StoreEnrollmentJTI(f.ctx, testValkeyRDB, f.jti(tokenA), f.connID, time.Minute); err != nil {
		t.Fatalf("restore token A entry: %v", err)
	}
	current, superseded := f.request(tokenB), f.request(tokenA)
	var supersededResp *pb.EnrollResponse
	var supersededErr error
	var done <-chan struct{}
	hook := &signHookPKI{Service: f.pki}
	hook.during = func() {
		done = runUntilBlockedOrDone(t, f.pool, func() {
			supersededResp, supersededErr = f.handler(f.pki).Enroll(f.ctx, superseded)
		})
	}
	resp, err := f.handler(hook).Enroll(f.ctx, current)
	<-done
	if supersededErr == nil {
		t.Fatalf("the superseded token also enrolled: two certificates (%s and %s), recorded %s",
			certSerial(t, supersededResp), certSerial(t, resp), f.row().serial)
	}
	f.enrolledAs(resp, err)
	refusedEnrollment(t, supersededResp, supersededErr, codes.PermissionDenied)
}

// Re-issue lands while an enrollment is being signed: the outcome is
// serialized. The enrollment holds the row, so it wins and the re-issue is
// refused; no token is minted for a connector that is no longer pending.
func TestEnrollBinding_ReissueRacingEnrollment(t *testing.T) {
	f := newBindingFixture(t)
	token := f.mustReissue()
	entriesBefore := f.tokenEntries()
	var reissued string
	var reissueCode int
	var done <-chan struct{}
	hook := &signHookPKI{Service: f.pki}
	hook.during = func() {
		done = runUntilBlockedOrDone(t, f.pool, func() {
			reissued, reissueCode = f.reissue(testValkeyRDB)
		})
	}
	resp, err := f.handler(hook).Enroll(f.ctx, f.request(token))
	<-done
	if reissued != "" {
		t.Fatalf("re-issue minted a token (status %d) while the enrollment it raced also succeeded (err %v)", reissueCode, err)
	}
	if reissueCode != http.StatusConflict {
		t.Fatalf("re-issue status %d, want 409", reissueCode)
	}
	f.enrolledAs(resp, err)
	// The refused re-issue left no entry; the enrollment consumed its own.
	if got := f.tokenEntries(); got != entriesBefore-1 {
		t.Fatalf("token entries %d → %d, want %d", entriesBefore, got, entriesBefore-1)
	}
}

// tokenEntries counts stored connector enrollment token entries.
func (f *bindingFixture) tokenEntries() int {
	f.t.Helper()
	keys, err := testValkeyRDB.Keys(f.ctx, enrollmentJTIPrefix+"*").Result()
	if err != nil {
		f.t.Fatalf("list token entries: %v", err)
	}
	return len(keys)
}

func TestEnrollBinding_ReissueRefusedForNonPendingConnector(t *testing.T) {
	f := newBindingFixture(t)
	token := f.mustReissue()
	f.exec(`UPDATE connectors SET status = 'active' WHERE id = $1`, f.connID)
	if reissued, code := f.reissue(testValkeyRDB); reissued != "" || code != http.StatusConflict {
		t.Fatalf("re-issue for an active connector: status %d, token minted %v", code, reissued != "")
	}
	if r := f.row(); r.jti != f.jti(token) || r.status != "active" {
		t.Fatalf("row changed by a refused re-issue: %+v", r)
	}
}

// The activation write fails: no certificate, nothing recorded, and the
// token is not consumed, so a retry succeeds once the database recovers.
func TestEnrollBinding_ActivationFailureReturnsNoCertificateAndKeepsToken(t *testing.T) {
	f := newBindingFixture(t)
	token := f.mustReissue()
	f.exec(`CREATE FUNCTION fail_activation() RETURNS trigger AS $$ BEGIN RAISE EXCEPTION 'activation refused'; END; $$ LANGUAGE plpgsql`)
	f.exec(`CREATE TRIGGER fail_activation BEFORE UPDATE ON connectors FOR EACH ROW
	        WHEN (NEW.status = 'active') EXECUTE FUNCTION fail_activation()`)
	resp, err := f.handler(f.pki).Enroll(f.ctx, f.request(token))
	refusedEnrollment(t, resp, err, codes.Internal)
	if r := f.row(); r.status != "pending" || r.serial != "" || r.jti != f.jti(token) {
		t.Fatalf("row after failed activation = %+v", r)
	}

	f.exec(`DROP TRIGGER fail_activation ON connectors`)
	resp, err = f.handler(f.pki).Enroll(f.ctx, f.request(token))
	f.enrolledAs(resp, err)
}

// The commit fails (deferred constraint): same guarantees.
func TestEnrollBinding_CommitFailureReturnsNoCertificateAndKeepsToken(t *testing.T) {
	f := newBindingFixture(t)
	token := f.mustReissue()
	f.exec(`CREATE FUNCTION fail_commit() RETURNS trigger AS $$ BEGIN RAISE EXCEPTION 'commit refused'; END; $$ LANGUAGE plpgsql`)
	f.exec(`CREATE CONSTRAINT TRIGGER fail_commit AFTER UPDATE ON connectors
	        DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
	        WHEN (NEW.status = 'active') EXECUTE FUNCTION fail_commit()`)
	resp, err := f.handler(f.pki).Enroll(f.ctx, f.request(token))
	refusedEnrollment(t, resp, err, codes.Internal)
	if r := f.row(); r.status != "pending" || r.serial != "" || r.jti != f.jti(token) {
		t.Fatalf("row after failed commit = %+v", r)
	}

	f.exec(`DROP TRIGGER fail_commit ON connectors`)
	resp, err = f.handler(f.pki).Enroll(f.ctx, f.request(token))
	f.enrolledAs(resp, err)
}

// PeekEnrollmentJTIForTest reports whether jti's Valkey entry exists, without
// consuming it.
func PeekEnrollmentJTIForTest(ctx context.Context, rdb valkeycompat.Cmdable, jti string) (string, bool, error) {
	val, err := rdb.Get(ctx, enrollmentJTIPrefix+jti).Result()
	if errors.Is(err, valkeycompat.Nil) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return val, true, nil
}
