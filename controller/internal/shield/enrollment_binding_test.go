package shield

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/valkey-io/valkey-go"
	"github.com/valkey-io/valkey-go/valkeycompat"
	shieldpb "github.com/yourorg/ztna/controller/gen/go/proto/shield/v1"
	"github.com/yourorg/ztna/controller/internal/appmeta"
	"github.com/yourorg/ztna/controller/internal/pki"
	"github.com/yourorg/ztna/controller/internal/tenant"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Certificate audit bug 8: re-issuing a shield enrollment token did not
// cancel the previous one, the minting update ignored whether a row changed,
// and Enroll never checked that the presented token was the shield's current
// one. Two valid tokens used at once could both be signed while only the last
// writer's serial was recorded, leaving an unrevocable certificate.

type shieldBindingFixture struct {
	t        *testing.T
	pool     *pgxpool.Pool
	ctx      context.Context
	pki      pki.Service
	redis    *miniredis.Miniredis
	rdb      valkeycompat.Cmdable
	cfg      Config
	wsID     string
	td       string
	rnID     string
	shieldID string
}

func newShieldBindingFixture(t *testing.T) *shieldBindingFixture {
	t.Helper()
	pool, ctx := setupHeartbeatTestDB(t)
	t.Setenv("PKI_MASTER_SECRET", "shield-binding-test-secret")
	pkiSvc, err := pki.Init(ctx, pool)
	if err != nil {
		t.Fatalf("pki.Init: %v", err)
	}
	server := miniredis.RunT(t)
	client, err := valkey.NewClient(valkey.ClientOption{InitAddress: []string{server.Addr()}, DisableCache: true})
	if err != nil {
		t.Fatalf("valkey client: %v", err)
	}
	t.Cleanup(client.Close)
	slug := fmt.Sprintf("sbind-%d", time.Now().UnixNano())
	f := &shieldBindingFixture{
		t: t, pool: pool, ctx: ctx, pki: pkiSvc, redis: server, rdb: valkeycompat.NewAdapter(client),
		cfg: Config{JWTSecret: "shield-binding-jwt", CertTTL: time.Hour, EnrollmentTokenTTL: 5 * time.Minute},
		td:  appmeta.WorkspaceTrustDomain(slug),
	}
	f.wsID = f.scalar(`INSERT INTO workspaces (slug, name, status, trust_domain) VALUES ($1, $1, 'active', $2) RETURNING id::text`, slug, f.td)
	ca, err := pkiSvc.GenerateWorkspaceCA(ctx, f.wsID)
	if err != nil {
		t.Fatalf("GenerateWorkspaceCA: %v", err)
	}
	f.exec(`INSERT INTO workspace_ca_keys
	          (tenant_id, encrypted_private_key, nonce, certificate_pem, key_algorithm, not_before, not_after)
	        VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		f.wsID, ca.EncryptedPrivateKey, ca.Nonce, ca.CertificatePEM, ca.KeyAlgorithm, ca.NotBefore, ca.NotAfter)
	f.rnID = f.scalar(`INSERT INTO remote_networks (tenant_id, name, location) VALUES ($1, 'rn', 'office') RETURNING id::text`, f.wsID)
	connID := f.scalar(`INSERT INTO connectors (tenant_id, remote_network_id, name, status, trust_domain, lan_addr)
		VALUES ($1, $2, 'conn', 'active', $3, '10.9.0.1:9091') RETURNING id::text`, f.wsID, f.rnID, f.td)
	f.shieldID = f.scalar(`INSERT INTO shields (tenant_id, remote_network_id, connector_id, name, status)
		VALUES ($1, $2, $3, 'binding-shield', 'pending') RETURNING id::text`, f.wsID, f.rnID, connID)
	return f
}

func (f *shieldBindingFixture) scalar(sql string, args ...any) string {
	f.t.Helper()
	var v string
	if err := f.pool.QueryRow(f.ctx, sql, args...).Scan(&v); err != nil {
		f.t.Fatalf("query %q: %v", sql, err)
	}
	return v
}

func (f *shieldBindingFixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx, sql, args...); err != nil {
		f.t.Fatalf("exec %q: %v", sql, err)
	}
}

func (f *shieldBindingFixture) svc(pkiSvc pki.Service, rdb valkeycompat.Cmdable) *service {
	return &service{cfg: f.cfg, db: f.pool, pki: pkiSvc, redis: rdb}
}

// reissue mints a token through the service, as the token endpoint and the
// GraphQL resolver do. Returns "" when refused.
func (f *shieldBindingFixture) reissue(rdb valkeycompat.Cmdable) (string, error) {
	token, _, err := f.svc(f.pki, rdb).GenerateShieldToken(f.ctx, f.rnID, f.wsID, f.wsID, f.shieldID, "binding-shield")
	return token, err
}

func (f *shieldBindingFixture) mustReissue() string {
	f.t.Helper()
	token, err := f.reissue(f.rdb)
	if err != nil || token == "" {
		f.t.Fatalf("re-issue: %v", err)
	}
	return token
}

func (f *shieldBindingFixture) jti(token string) string {
	f.t.Helper()
	claims, err := VerifyShieldToken(f.cfg, token)
	if err != nil {
		f.t.Fatalf("verify token: %v", err)
	}
	return claims.ID
}

func (f *shieldBindingFixture) request(token string) *shieldpb.EnrollRequest {
	f.t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		f.t.Fatalf("generate key: %v", err)
	}
	uri, _ := url.Parse(appmeta.ShieldSPIFFEID(f.td, f.shieldID))
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: "shield-" + f.shieldID},
		URIs:    []*url.URL{uri},
	}, key)
	if err != nil {
		f.t.Fatalf("create CSR: %v", err)
	}
	return &shieldpb.EnrollRequest{EnrollmentToken: token, CsrDer: csr, Version: "1.0.0", Hostname: "binding-host"}
}

type shieldRow struct{ status, serial, jti string }

func (f *shieldBindingFixture) row() shieldRow {
	f.t.Helper()
	var r shieldRow
	if err := f.pool.QueryRow(f.ctx,
		`SELECT status, COALESCE(cert_serial, ''), COALESCE(enrollment_token_jti, '') FROM shields WHERE id = $1`,
		f.shieldID).Scan(&r.status, &r.serial, &r.jti); err != nil {
		f.t.Fatalf("read shield: %v", err)
	}
	return r
}

func shieldCertSerial(t *testing.T, resp *shieldpb.EnrollResponse) string {
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

func (f *shieldBindingFixture) enrolledAs(resp *shieldpb.EnrollResponse, err error) {
	f.t.Helper()
	if err != nil {
		f.t.Fatalf("enroll: %v", err)
	}
	r := f.row()
	if r.status != "active" || r.jti != "" {
		f.t.Fatalf("row after enrollment = %+v, want active with the token consumed", r)
	}
	if got := shieldCertSerial(f.t, resp); got != r.serial {
		f.t.Fatalf("returned certificate serial %s is not the recorded serial %s", got, r.serial)
	}
}

func refusedShieldEnrollment(t *testing.T, resp *shieldpb.EnrollResponse, err error, code codes.Code) {
	t.Helper()
	if got := status.Code(err); got != code {
		t.Fatalf("code = %v (err %v), want %v", got, err, code)
	}
	if resp != nil {
		t.Fatal("a certificate was returned")
	}
}

type shieldSignHookPKI struct {
	pki.Service
	once   sync.Once
	during func()
}

func (p *shieldSignHookPKI) SignShieldCert(ctx context.Context, tenantID, shieldID, trustDomain string, csr *x509.CertificateRequest, ttl time.Duration) (*pki.ShieldCertResult, error) {
	if p.during != nil {
		p.once.Do(p.during)
	}
	return p.Service.SignShieldCert(ctx, tenantID, shieldID, trustDomain, csr, ttl)
}

// untilBlockedOrDone starts fn and returns once it has finished or is
// waiting on a database row lock. Polls; no fixed sleeps.
func untilBlockedOrDone(t *testing.T, pool *pgxpool.Pool, fn func()) <-chan struct{} {
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

func (f *shieldBindingFixture) hasEntry(token string) bool {
	return f.redis.Exists(enrollmentJTIPrefix + f.jti(token))
}

func TestShieldEnrollBinding_NormalEnrollment(t *testing.T) {
	f := newShieldBindingFixture(t)
	token := f.mustReissue()
	if got := f.row().jti; got != f.jti(token) {
		t.Fatalf("row token %q, want %q", got, f.jti(token))
	}
	resp, err := f.svc(f.pki, f.rdb).Enroll(f.ctx, f.request(token))
	f.enrolledAs(resp, err)
	if f.hasEntry(token) {
		t.Fatal("the token's Valkey entry survived a successful enrollment")
	}
	resp, err = f.svc(f.pki, f.rdb).Enroll(f.ctx, f.request(token))
	refusedShieldEnrollment(t, resp, err, codes.PermissionDenied)
}

// mintChain mints A, then B, then C. Each re-issue gives the pending shield
// the first free interface address other than its own, so C gets A's address
// back: the old interface-address comparison then accepts A again. Only the
// token binding tells A and C apart.
func (f *shieldBindingFixture) mintChain(rdb valkeycompat.Cmdable) (tokenA, tokenC string) {
	f.t.Helper()
	tokenA = f.mustReissue()
	addrA := f.scalar(`SELECT interface_addr FROM shields WHERE id = $1`, f.shieldID)
	for _, step := range []string{"B", "C"} {
		token, err := f.reissue(rdb)
		if err != nil || token == "" {
			f.t.Fatalf("re-issue %s: %v", step, err)
		}
		tokenC = token
	}
	if got := f.scalar(`SELECT interface_addr FROM shields WHERE id = $1`, f.shieldID); got != addrA {
		f.t.Fatalf("test setup: C's interface address %s is not A's %s", got, addrA)
	}
	return tokenA, tokenC
}

func TestShieldEnrollBinding_SupersededTokenRejected(t *testing.T) {
	f := newShieldBindingFixture(t)
	tokenA, tokenB := f.mintChain(f.rdb)
	if err := StoreShieldJTI(f.ctx, f.rdb, f.jti(tokenA), f.shieldID, time.Minute); err != nil {
		t.Fatalf("restore token A entry: %v", err)
	}
	resp, err := f.svc(f.pki, f.rdb).Enroll(f.ctx, f.request(tokenA))
	refusedShieldEnrollment(t, resp, err, codes.PermissionDenied)
	if r := f.row(); r.status != "pending" || r.jti != f.jti(tokenB) {
		t.Fatalf("row after refused superseded token = %+v", r)
	}
	resp, err = f.svc(f.pki, f.rdb).Enroll(f.ctx, f.request(tokenB))
	f.enrolledAs(resp, err)
}

func TestShieldEnrollBinding_ReissueDeletesPredecessorEntry(t *testing.T) {
	f := newShieldBindingFixture(t)
	tokenA := f.mustReissue()
	tokenB := f.mustReissue()
	if f.hasEntry(tokenA) {
		t.Fatal("re-issue left the predecessor's Valkey entry behind")
	}
	if !f.hasEntry(tokenB) {
		t.Fatal("re-issue did not store the new token's entry")
	}
}

func TestShieldEnrollBinding_PredecessorCleanupFailureStillRejectsOldToken(t *testing.T) {
	f := newShieldBindingFixture(t)
	// Every predecessor cleanup fails (best effort, so re-issue still works).
	tokenA, tokenB := f.mintChain(shieldFailingDel{f.rdb})
	if !f.hasEntry(tokenA) {
		t.Fatal("test setup: token A's entry should have survived the failed cleanup")
	}
	resp, err := f.svc(f.pki, f.rdb).Enroll(f.ctx, f.request(tokenA))
	refusedShieldEnrollment(t, resp, err, codes.PermissionDenied)
	resp, err = f.svc(f.pki, f.rdb).Enroll(f.ctx, f.request(tokenB))
	f.enrolledAs(resp, err)
}

type shieldFailingDel struct{ valkeycompat.Cmdable }

func (shieldFailingDel) Del(context.Context, ...string) *valkeycompat.IntCmd {
	cmd := &valkeycompat.IntCmd{}
	cmd.SetErr(errors.New("valkey unavailable"))
	return cmd
}

func TestShieldEnrollBinding_ConcurrentSameToken(t *testing.T) {
	f := newShieldBindingFixture(t)
	token := f.mustReissue()
	first, second := f.request(token), f.request(token)
	var secondResp *shieldpb.EnrollResponse
	var secondErr error
	var done <-chan struct{}
	hook := &shieldSignHookPKI{Service: f.pki}
	hook.during = func() {
		done = untilBlockedOrDone(t, f.pool, func() {
			secondResp, secondErr = f.svc(f.pki, f.rdb).Enroll(f.ctx, second)
		})
	}
	resp, err := f.svc(hook, f.rdb).Enroll(f.ctx, first)
	<-done
	f.enrolledAs(resp, err)
	refusedShieldEnrollment(t, secondResp, secondErr, codes.PermissionDenied)
}

func TestShieldEnrollBinding_ConcurrentSupersededAndCurrentTokens(t *testing.T) {
	f := newShieldBindingFixture(t)
	tokenA, tokenB := f.mintChain(f.rdb)
	if err := StoreShieldJTI(f.ctx, f.rdb, f.jti(tokenA), f.shieldID, time.Minute); err != nil {
		t.Fatalf("restore token A entry: %v", err)
	}
	current, superseded := f.request(tokenB), f.request(tokenA)
	var supersededResp *shieldpb.EnrollResponse
	var supersededErr error
	var done <-chan struct{}
	hook := &shieldSignHookPKI{Service: f.pki}
	hook.during = func() {
		done = untilBlockedOrDone(t, f.pool, func() {
			supersededResp, supersededErr = f.svc(f.pki, f.rdb).Enroll(f.ctx, superseded)
		})
	}
	resp, err := f.svc(hook, f.rdb).Enroll(f.ctx, current)
	<-done
	if supersededErr == nil {
		t.Fatalf("the superseded token also enrolled: two certificates (%s and %s), recorded %s",
			shieldCertSerial(t, supersededResp), shieldCertSerial(t, resp), f.row().serial)
	}
	f.enrolledAs(resp, err)
	refusedShieldEnrollment(t, supersededResp, supersededErr, codes.PermissionDenied)
}

func TestShieldEnrollBinding_ReissueRacingEnrollment(t *testing.T) {
	f := newShieldBindingFixture(t)
	token := f.mustReissue()
	var reissued string
	var reissueErr error
	var done <-chan struct{}
	hook := &shieldSignHookPKI{Service: f.pki}
	hook.during = func() {
		done = untilBlockedOrDone(t, f.pool, func() {
			reissued, reissueErr = f.reissue(f.rdb)
		})
	}
	resp, err := f.svc(hook, f.rdb).Enroll(f.ctx, f.request(token))
	<-done
	if reissued != "" {
		t.Fatalf("re-issue minted a token while the enrollment it raced also succeeded (err %v)", err)
	}
	if reissueErr == nil {
		t.Fatal("re-issue for a shield that is no longer pending reported success")
	}
	f.enrolledAs(resp, err)
}

// The token endpoint passes its pending pre-check, then the shield enrolls
// before the swap: the endpoint answers 409 and mints nothing.
func TestShieldEnrollBinding_TokenEndpointRacingEnrollment(t *testing.T) {
	f := newShieldBindingFixture(t)
	token := f.mustReissue()
	var code int
	var body string
	var done <-chan struct{}
	hook := &shieldSignHookPKI{Service: f.pki}
	hook.during = func() {
		done = untilBlockedOrDone(t, f.pool, func() {
			req := httptest.NewRequest(http.MethodPost, "/api/shields/"+f.shieldID+"/token", nil)
			req = req.WithContext(tenant.Set(f.ctx, tenant.TenantContext{TenantID: f.wsID}))
			rec := httptest.NewRecorder()
			f.svc(f.pki, f.rdb).TokenHandler().ServeHTTP(rec, req)
			code, body = rec.Code, rec.Body.String()
		})
	}
	resp, err := f.svc(hook, f.rdb).Enroll(f.ctx, f.request(token))
	<-done
	if code != http.StatusConflict {
		t.Fatalf("token endpoint status %d (%q), want 409", code, body)
	}
	f.enrolledAs(resp, err)
}

// A non-pending shield whose row still names the token is refused.
func TestShieldEnrollBinding_NonPendingShieldRefused(t *testing.T) {
	f := newShieldBindingFixture(t)
	token := f.mustReissue()
	f.exec(`UPDATE shields SET status = 'disconnected' WHERE id = $1`, f.shieldID)
	resp, err := f.svc(f.pki, f.rdb).Enroll(f.ctx, f.request(token))
	refusedShieldEnrollment(t, resp, err, codes.PermissionDenied)
	if r := f.row(); r.status != "disconnected" || r.serial != "" {
		t.Fatalf("row changed by a refused enrollment: %+v", r)
	}
}

// The current token whose Valkey entry expired is refused.
func TestShieldEnrollBinding_ExpiredEntryRefused(t *testing.T) {
	f := newShieldBindingFixture(t)
	token := f.mustReissue()
	f.redis.Del(enrollmentJTIPrefix + f.jti(token))
	resp, err := f.svc(f.pki, f.rdb).Enroll(f.ctx, f.request(token))
	refusedShieldEnrollment(t, resp, err, codes.PermissionDenied)
	if !strings.Contains(err.Error(), "token expired or already used") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
	if r := f.row(); r.status != "pending" || r.jti != f.jti(token) {
		t.Fatalf("row changed by a refused enrollment: %+v", r)
	}
}

func TestShieldEnrollBinding_ReissueRefusedForNonPendingShield(t *testing.T) {
	f := newShieldBindingFixture(t)
	token := f.mustReissue()
	f.exec(`UPDATE shields SET status = 'active' WHERE id = $1`, f.shieldID)
	keysBefore := len(f.redis.Keys())
	reissued, err := f.reissue(f.rdb)
	if err == nil || reissued != "" {
		t.Fatalf("re-issue for an active shield: err %v, token minted %v", err, reissued != "")
	}
	if r := f.row(); r.jti != f.jti(token) || r.status != "active" {
		t.Fatalf("row changed by a refused re-issue: %+v", r)
	}
	if got := len(f.redis.Keys()); got != keysBefore {
		t.Fatalf("Valkey entries %d → %d: a refused re-issue left a token entry", keysBefore, got)
	}
}

func TestShieldEnrollBinding_ActivationFailureReturnsNoCertificateAndKeepsToken(t *testing.T) {
	f := newShieldBindingFixture(t)
	token := f.mustReissue()
	f.exec(`CREATE FUNCTION fail_shield_activation() RETURNS trigger AS $$ BEGIN RAISE EXCEPTION 'activation refused'; END; $$ LANGUAGE plpgsql`)
	f.exec(`CREATE TRIGGER fail_shield_activation BEFORE UPDATE ON shields FOR EACH ROW
	        WHEN (NEW.status = 'active') EXECUTE FUNCTION fail_shield_activation()`)
	resp, err := f.svc(f.pki, f.rdb).Enroll(f.ctx, f.request(token))
	refusedShieldEnrollment(t, resp, err, codes.Internal)
	if r := f.row(); r.status != "pending" || r.serial != "" || r.jti != f.jti(token) {
		t.Fatalf("row after failed activation = %+v", r)
	}
	f.exec(`DROP TRIGGER fail_shield_activation ON shields`)
	resp, err = f.svc(f.pki, f.rdb).Enroll(f.ctx, f.request(token))
	f.enrolledAs(resp, err)
}

func TestShieldEnrollBinding_CommitFailureReturnsNoCertificateAndKeepsToken(t *testing.T) {
	f := newShieldBindingFixture(t)
	token := f.mustReissue()
	f.exec(`CREATE FUNCTION fail_shield_commit() RETURNS trigger AS $$ BEGIN RAISE EXCEPTION 'commit refused'; END; $$ LANGUAGE plpgsql`)
	f.exec(`CREATE CONSTRAINT TRIGGER fail_shield_commit AFTER UPDATE ON shields
	        DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
	        WHEN (NEW.status = 'active') EXECUTE FUNCTION fail_shield_commit()`)
	resp, err := f.svc(f.pki, f.rdb).Enroll(f.ctx, f.request(token))
	refusedShieldEnrollment(t, resp, err, codes.Internal)
	if r := f.row(); r.status != "pending" || r.serial != "" || r.jti != f.jti(token) {
		t.Fatalf("row after failed commit = %+v", r)
	}
	f.exec(`DROP TRIGGER fail_shield_commit ON shields`)
	resp, err = f.svc(f.pki, f.rdb).Enroll(f.ctx, f.request(token))
	f.enrolledAs(resp, err)
}
