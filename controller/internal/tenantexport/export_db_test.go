package tenantexport_test

// Sprint 23 S4 (2026-11-18 design): the tenant's own log download during the
// deletion grace. DB-backed: must run, not skip, in the gate.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourorg/ztna/controller/internal/tenantexport"
)

func newDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	adminDSN := os.Getenv("PKI_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("PKI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	name := fmt.Sprintf("tenantexport_test_%d", time.Now().UnixNano())
	admin, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(adminDSN)
	u.Path = "/" + name
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		admin.Close()
	})
	files, _ := filepath.Glob(filepath.Join("..", "..", "migrations", "*.sql"))
	sort.Strings(files)
	for _, f := range files {
		b, _ := os.ReadFile(f)
		if _, err := pool.Exec(ctx, string(b)); err != nil {
			t.Fatalf("apply %s: %v", filepath.Base(f), err)
		}
	}
	return pool
}

type fixture struct {
	pool          *pgxpool.Pool
	ws, admin, mb string
	mux           *http.ServeMux
}

func setup(t *testing.T) *fixture {
	pool := newDB(t)
	ctx := context.Background()
	f := &fixture{pool: pool}
	q := func(dst *string, sql string, args ...any) {
		t.Helper()
		if err := pool.QueryRow(ctx, sql, args...).Scan(dst); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	q(&f.ws, `INSERT INTO workspaces (slug, name, status, trust_domain, suspended_at, suspended_by, suspension_reason)
		VALUES ('acme', 'Acme', 'active', 'acme.test', NOW(), 'op', 'r') RETURNING id::text`)
	q(&f.admin, `INSERT INTO users (tenant_id, email, provider, provider_sub, role) VALUES ($1, 'admin@acme.test', 'google', 'a', 'admin') RETURNING id::text`, f.ws)
	q(&f.mb, `INSERT INTO users (tenant_id, email, provider, provider_sub, role) VALUES ($1, 'm@acme.test', 'google', 'm', 'member') RETURNING id::text`, f.ws)
	if _, err := pool.Exec(ctx, `UPDATE workspaces SET delete_scheduled_at = NOW(), delete_after = NOW() + INTERVAL '30 days', delete_by = 'op', delete_reason = 'r' WHERE id = $1`, f.ws); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO audit_logs (tenant_id, actor_email, action, target_type, target_id) VALUES ($1, 'a@x', 'resource.create', 'resource', 'r1')`, f.ws); err != nil {
		t.Fatal(err)
	}
	h := &tenantexport.Handler{Pool: pool}
	f.mux = http.NewServeMux()
	f.mux.HandleFunc("POST /api/tenant-export/info", h.Info)
	f.mux.HandleFunc("POST /api/tenant-export/download", h.Download)
	return f
}

func (f *fixture) post(path, token string) *httptest.ResponseRecorder {
	b, _ := json.Marshal(map[string]string{"token": token})
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b)))
	return w
}

func (f *fixture) emailToken(t *testing.T) string {
	t.Helper()
	var toks []tenantexport.IssuedToken
	err := pgx.BeginTxFunc(context.Background(), f.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		var err error
		toks, err = tenantexport.IssueEmailTokensTx(context.Background(), tx, f.ws, time.Now().Add(30*24*time.Hour))
		return err
	})
	if err != nil || len(toks) != 1 || toks[0].UserID != f.admin {
		t.Fatalf("email tokens: %+v %v (want one, for the admin only)", toks, err)
	}
	return toks[0].Token
}

func archiveFiles(t *testing.T, body []byte) map[string]string {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	out := map[string]string{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		c, _ := io.ReadAll(tr)
		out[h.Name] = string(c)
	}
	return out
}

// An emailed link: info and download work repeatedly until the deletion is
// cancelled; the download is the tenant's archive and is audited without
// the token; only the token's hash is stored.
func TestExportDB_EmailLink(t *testing.T) {
	f := setup(t)
	tok := f.emailToken(t)
	var stored int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM tenant_export_tokens WHERE position($1::bytea in token_sha256) > 0`, []byte(tok)).Scan(&stored); err != nil || stored != 0 {
		t.Fatalf("raw token stored: %d %v", stored, err)
	}
	if w := f.post("/api/tenant-export/info", tok); w.Code != 200 || !strings.Contains(w.Body.String(), `"workspace_name":"Acme"`) {
		t.Fatalf("info: %d %s", w.Code, w.Body.String())
	}
	for i := 0; i < 2; i++ {
		w := f.post("/api/tenant-export/download", tok)
		if w.Code != 200 || w.Header().Get("Content-Type") != "application/gzip" || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("download %d: %d %v", i, w.Code, w.Header())
		}
		files := archiveFiles(t, w.Body.Bytes())
		if !strings.Contains(files["audit_log.jsonl"], "resource.create") || files["manifest.json"] == "" || len(files) != 7 {
			t.Fatalf("archive files = %v", len(files))
		}
	}
	var n int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM audit_logs WHERE tenant_id = $1 AND action = 'tenant_export.download'
		AND position($2 in details::text) = 0`, f.ws, tok).Scan(&n); err != nil || n != 2 {
		t.Fatalf("download audits without token = %d %v", n, err)
	}
	// Cancelled deletion: the link stops working.
	if _, err := f.pool.Exec(context.Background(), `UPDATE workspaces SET delete_scheduled_at = NULL, delete_after = NULL, delete_by = NULL, delete_reason = NULL WHERE id = $1`, f.ws); err != nil {
		t.Fatal(err)
	}
	if w := f.post("/api/tenant-export/download", tok); w.Code != 404 {
		t.Fatalf("download after cancel: %d", w.Code)
	}
}

// A sign-in token: admins only, single use; the purge start ends every link;
// garbage tokens get the same 404.
func TestExportDB_SigninTokenAndLimits(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	if _, err := tenantexport.IssueSigninToken(ctx, f.pool, f.ws, f.mb); err != tenantexport.ErrInvalidToken {
		t.Fatalf("sign-in token for a member: %v", err)
	}
	st, err := tenantexport.IssueSigninToken(ctx, f.pool, f.ws, f.admin)
	if err != nil || time.Until(st.ExpiresAt) > tenantexport.SigninTokenTTL+time.Minute {
		t.Fatalf("sign-in token: %+v %v", st, err)
	}
	if w := f.post("/api/tenant-export/info", st.Token); w.Code != 200 {
		t.Fatalf("info (does not consume): %d", w.Code)
	}
	if w := f.post("/api/tenant-export/download", st.Token); w.Code != 200 {
		t.Fatalf("first download: %d", w.Code)
	}
	if w := f.post("/api/tenant-export/download", st.Token); w.Code != 404 {
		t.Fatalf("second download with a sign-in token: %d, want 404", w.Code)
	}
	tok := f.emailToken(t)
	if _, err := f.pool.Exec(ctx, `INSERT INTO tenant_purges (workspace_id, slug, delete_by, delete_reason, scheduled_at) VALUES ($1, 'acme', 'op', 'r', NOW())`, f.ws); err != nil {
		t.Fatal(err)
	}
	if w := f.post("/api/tenant-export/download", tok); w.Code != 404 {
		t.Fatalf("download after the purge started: %d", w.Code)
	}
	if _, err := tenantexport.IssueSigninToken(ctx, f.pool, f.ws, f.admin); err != tenantexport.ErrInvalidToken {
		t.Fatalf("sign-in token after the purge started: %v", err)
	}
	for _, bad := range []string{"x", strings.Repeat("A", 43), ""} {
		if w := f.post("/api/tenant-export/download", bad); w.Code != 404 && w.Code != 400 {
			t.Fatalf("bad token %q: %d", bad, w.Code)
		}
	}
	// A suspended tenant without a scheduled deletion gets no sign-in token.
	if _, err := f.pool.Exec(ctx, `DELETE FROM tenant_purges; UPDATE workspaces SET delete_scheduled_at = NULL, delete_after = NULL, delete_by = NULL, delete_reason = NULL WHERE id = '`+f.ws+`'`); err != nil {
		t.Fatal(err)
	}
	if _, err := tenantexport.IssueSigninToken(ctx, f.pool, f.ws, f.admin); err != tenantexport.ErrInvalidToken {
		t.Fatalf("sign-in token without a scheduled deletion: %v", err)
	}
}

// A legal hold keeps a tenant past its scheduled deletion: its data is still
// there, so the emailed link keeps working and admins can still get a
// sign-in download, until the purge starts.
func TestExportDB_LinksWorkUnderLegalHoldPastTheGrace(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	tok := f.emailToken(t)
	if _, err := f.pool.Exec(ctx, `UPDATE workspaces SET delete_after = NOW() - INTERVAL '1 hour',
		legal_hold = true, legal_hold_reason = 'r', legal_hold_by = 'op', legal_hold_at = NOW() WHERE id = $1`, f.ws); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE tenant_export_tokens SET expires_at = NOW() - INTERVAL '1 hour' WHERE workspace_id = $1`, f.ws); err != nil {
		t.Fatal(err)
	}
	if w := f.post("/api/tenant-export/download", tok); w.Code != 200 {
		t.Fatalf("email link under a legal hold past the grace: %d", w.Code)
	}
	if _, err := tenantexport.IssueSigninToken(ctx, f.pool, f.ws, f.admin); err != nil {
		t.Fatalf("sign-in token under a legal hold past the grace: %v", err)
	}
}
