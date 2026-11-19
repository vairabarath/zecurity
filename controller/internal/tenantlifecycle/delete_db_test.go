package tenantlifecycle

// Sprint 23 S4 (D-46): tenant deletion API and the purge. DB-backed: must
// run, not skip, in the gate.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/yourorg/ztna/controller/internal/auditexport"
	"github.com/yourorg/ztna/controller/internal/provider"
	"github.com/yourorg/ztna/controller/internal/tenantexport"
)

type fakeMailer struct {
	mu   sync.Mutex
	sent []string // "to|body"
	done chan struct{}
}

func (m *fakeMailer) SendPlain(to, _, body string) error {
	m.mu.Lock()
	m.sent = append(m.sent, to+"|"+body)
	m.mu.Unlock()
	m.done <- struct{}{}
	return nil
}

func (e *env) doDelete(t *testing.T, role, method, path string, body any) resp {
	t.Helper()
	b, _ := json.Marshal(body)
	r := httptest.NewRequest(method, path, bytes.NewReader(b))
	r.Header.Set("Content-Type", "application/json")
	r = r.WithContext(provider.WithActor(r.Context(), provider.Actor{UserID: e.operator[role], Email: role + "@provider.test", Role: role}))
	mux := http.NewServeMux()
	mux.HandleFunc("POST /provider/tenants/{id}/suspend", e.h.Suspend)
	mux.HandleFunc("POST /provider/tenants/{id}/resume", e.h.Resume)
	mux.HandleFunc("POST /provider/tenants/{id}/delete", e.h.Delete)
	mux.HandleFunc("POST /provider/tenants/{id}/delete/cancel", e.h.CancelDelete)
	mux.HandleFunc("PUT /provider/tenants/{id}/legal-hold", e.h.LegalHold)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return resp{w.Code, w.Body.String()}
}

func errCode(r resp) string {
	var v struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal([]byte(r.body), &v)
	return v.Error
}

// adminTenant seeds a tenant whose first user is an admin.
func (e *env) adminTenant(t *testing.T, slug string) (string, []string) {
	t.Helper()
	id, users := e.tenant(t, slug)
	if _, err := e.pool.Exec(context.Background(), `UPDATE users SET role = 'admin' WHERE id = $1`, users[0]); err != nil {
		t.Fatal(err)
	}
	return id, users
}

func TestDeleteDB_ScheduleCancelLegalHold(t *testing.T) {
	e := newEnv(t)
	mail := &fakeMailer{done: make(chan struct{}, 8)}
	e.h.Mailer, e.h.AppBaseURL = mail, "https://admin.test"
	id, users := e.adminTenant(t, "acme")
	base := "/provider/tenants/" + id
	del := map[string]any{"reason": "contract ended", "confirm_slug": "acme"}

	// D-10: only a suspended tenant.
	if r := e.doDelete(t, provider.RoleSuperAdmin, "POST", base+"/delete", del); r.code != 409 || errCode(r) != "not_suspended" {
		t.Fatalf("delete of an active tenant: %d %s", r.code, r.body)
	}
	if r := e.doDelete(t, provider.RoleSuperAdmin, "POST", base+"/suspend", map[string]any{"reason": "r", "confirm_slug": "acme"}); r.code != 204 {
		t.Fatalf("suspend: %d %s", r.code, r.body)
	}
	if r := e.doDelete(t, provider.RoleRelayOps, "POST", base+"/delete", del); r.code != 403 {
		t.Fatalf("relay-ops: %d", r.code)
	}
	if r := e.doDelete(t, provider.RoleSuperAdmin, "POST", base+"/delete", map[string]any{"reason": "r", "confirm_slug": "acm"}); errCode(r) != "confirmation_mismatch" {
		t.Fatalf("wrong slug: %d %s", r.code, r.body)
	}
	if r := e.doDelete(t, provider.RoleSuperAdmin, "POST", base+"/delete", map[string]any{"confirm_slug": "acme"}); errCode(r) != "reason_required" {
		t.Fatalf("no reason: %d %s", r.code, r.body)
	}
	r := e.doDelete(t, provider.RoleSuperAdmin, "POST", base+"/delete", del)
	if r.code != 200 {
		t.Fatalf("delete: %d %s", r.code, r.body)
	}
	var after time.Time
	e.scalar(t, &after, `SELECT delete_after FROM workspaces WHERE id = $1`, id)
	if d := time.Until(after); d < 29*24*time.Hour || d > 31*24*time.Hour {
		t.Fatalf("delete_after in %v, want 30 days", d)
	}
	// One emailed link per active admin; the token is never in the audit.
	select {
	case <-mail.done:
	case <-time.After(5 * time.Second):
		t.Fatal("no download email")
	}
	mail.mu.Lock()
	sent := append([]string(nil), mail.sent...)
	mail.mu.Unlock()
	var adminEmail string
	e.scalar(t, &adminEmail, `SELECT email FROM users WHERE id = $1`, users[0])
	if len(sent) != 1 || !strings.HasPrefix(sent[0], adminEmail+"|") || !strings.Contains(sent[0], "https://admin.test/tenant-export#token=") {
		t.Fatalf("emails = %q", sent)
	}
	tok := sent[0][strings.Index(sent[0], "#token=")+7:]
	tok = strings.Fields(tok)[0]
	var n int
	e.scalar(t, &n, `SELECT count(*) FROM provider_audit_logs WHERE position($1 in details::text) > 0`, tok)
	if n != 0 {
		t.Fatal("download token in the provider audit")
	}
	e.scalar(t, &n, `SELECT count(*) FROM tenant_export_tokens WHERE workspace_id = $1 AND purpose = 'email' AND user_id = $2`, id, users[0])
	if n != 1 {
		t.Fatalf("email tokens = %d, want 1", n)
	}

	// Resume and a second schedule are refused while scheduled.
	if r := e.doDelete(t, provider.RoleSuperAdmin, "POST", base+"/resume", map[string]any{"reason": "r"}); errCode(r) != "delete_scheduled" {
		t.Fatalf("resume while scheduled: %d %s", r.code, r.body)
	}
	if r := e.doDelete(t, provider.RoleSuperAdmin, "POST", base+"/delete", del); errCode(r) != "delete_scheduled" {
		t.Fatalf("second schedule: %d %s", r.code, r.body)
	}
	// Legal hold on, on again (unchanged), off.
	on, off := true, false
	if r := e.doDelete(t, provider.RoleSuperAdmin, "PUT", base+"/legal-hold", map[string]any{"on": on, "reason": "litigation"}); r.code != 204 {
		t.Fatalf("hold on: %d %s", r.code, r.body)
	}
	if r := e.doDelete(t, provider.RoleSuperAdmin, "PUT", base+"/legal-hold", map[string]any{"on": on, "reason": "x"}); errCode(r) != "legal_hold_unchanged" {
		t.Fatalf("hold on twice: %d %s", r.code, r.body)
	}
	if r := e.doDelete(t, provider.RoleSuperAdmin, "PUT", base+"/legal-hold", map[string]any{"on": off, "reason": "settled"}); r.code != 204 {
		t.Fatalf("hold off: %d %s", r.code, r.body)
	}
	// Cancel: links dropped; then resume works.
	if r := e.doDelete(t, provider.RoleSuperAdmin, "POST", base+"/delete/cancel", map[string]any{"reason": "customer renewed"}); r.code != 204 {
		t.Fatalf("cancel: %d %s", r.code, r.body)
	}
	if r := e.doDelete(t, provider.RoleSuperAdmin, "POST", base+"/delete/cancel", map[string]any{"reason": "r"}); errCode(r) != "delete_not_scheduled" {
		t.Fatalf("cancel twice: %d %s", r.code, r.body)
	}
	e.scalar(t, &n, `SELECT count(*) FROM tenant_export_tokens WHERE workspace_id = $1`, id)
	if n != 0 {
		t.Fatalf("tokens after cancel = %d", n)
	}
	if r := e.doDelete(t, provider.RoleSuperAdmin, "POST", base+"/resume", map[string]any{"reason": "r"}); r.code != 204 {
		t.Fatalf("resume after cancel: %d %s", r.code, r.body)
	}
	for _, a := range []string{provider.ActionTenantDelete, provider.ActionTenantDeleteCancel, provider.ActionTenantLegalHoldSet} {
		if len(e.auditRows(t, a)) == 0 {
			t.Errorf("no %s audit row", a)
		}
	}
	d := e.auditRows(t, provider.ActionTenantDelete)[0]
	if d["reason"] != "contract ended" || d["before"] == nil || d["after"] == nil {
		t.Fatalf("delete audit = %v", d)
	}
}

// due seeds a suspended tenant with data, schedules its deletion in the past
// and returns its id.
func (e *env) due(t *testing.T, slug string) string {
	t.Helper()
	ctx := context.Background()
	id, users := e.adminTenant(t, slug)
	stmts := []string{
		`UPDATE workspaces SET suspended_at = NOW(), suspended_by = 'op', suspension_reason = 'r' WHERE id = $1`,
		`INSERT INTO audit_logs (tenant_id, actor_email, action, target_type, target_id) VALUES ($1, 'a@x', 'resource.create', 'resource', 'r1')`,
		`INSERT INTO workspace_ca_keys (tenant_id, encrypted_private_key, nonce, key_algorithm, certificate_pem, not_before, not_after)
		 VALUES ($1, 'k', 'n', 'EC-P384', 'PEM', NOW(), NOW() + INTERVAL '1 year')`,
		`INSERT INTO remote_networks (tenant_id, name, location) VALUES ($1, 'net', 'office')`,
		`INSERT INTO outbox_events (event_type, workspace_id, payload, correlation_id) VALUES ('test', $1, '{}', gen_random_uuid())`,
	}
	for _, q := range stmts {
		if _, err := e.pool.Exec(ctx, q, id); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	var conn string
	e.scalar(t, &conn, `INSERT INTO connectors (tenant_id, remote_network_id, name, status, trust_domain)
		SELECT $1, id, 'c1', 'active', $2 FROM remote_networks WHERE tenant_id = $1 RETURNING id::text`, id, slug+".zecurity.test")
	if _, err := e.pool.Exec(ctx, `INSERT INTO connector_certificates (tenant_id, connector_id, serial, issued_at, not_after)
		VALUES ($1, $2, $3, NOW(), NOW() + INTERVAL '1 day')`, id, conn, "serial-"+slug); err != nil {
		t.Fatal(err)
	}
	if _, err := e.h.Store.ScheduleDelete(ctx, id, slug, "op@provider.test", "contract ended", -time.Second, nil); err != nil {
		t.Fatalf("schedule: %v", err)
	}
	_ = users
	return id
}

func (e *env) purger(t *testing.T, dir string) *Purger {
	return &Purger{Pool: e.pool, ProviderStore: provider.NewStore(e.pool), ExportDir: dir,
		Sessions: e.sessions, Policy: e.policy, PublishRelayLists: func(context.Context) error { return nil }}
}

func readArchive(t *testing.T, path string) map[string]string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	out := map[string]string{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
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

// assertPurged checks the end state of a completed purge.
func (e *env) assertPurged(t *testing.T, id, dir string, providerRowsBefore int) {
	t.Helper()
	var n int
	for _, q := range []string{
		`SELECT count(*) FROM workspaces WHERE id = $1`,
		`SELECT count(*) FROM audit_logs WHERE tenant_id = $1`,
		`SELECT count(*) FROM workspace_ca_keys WHERE tenant_id = $1`,
		`SELECT count(*) FROM connectors WHERE tenant_id = $1`,
		`SELECT count(*) FROM users WHERE tenant_id = $1`,
		`SELECT count(*) FROM outbox_events WHERE workspace_id = $1`,
	} {
		e.scalar(t, &n, q, id)
		if n != 0 {
			t.Fatalf("after purge %q = %d, want 0", q, n)
		}
	}
	var kind, loc, sha, step string
	e.scalar(t, &step, `SELECT step FROM tenant_purges WHERE workspace_id = $1`, id)
	if step != "done" {
		t.Fatalf("purge step = %s", step)
	}
	if err := e.pool.QueryRow(context.Background(), `SELECT export_kind, export_location, export_sha256 FROM workspace_tombstones WHERE workspace_id = $1`, id).
		Scan(&kind, &loc, &sha); err != nil {
		t.Fatalf("tombstone: %v", err)
	}
	files, _ := filepath.Glob(filepath.Join(dir, id, "*"))
	if kind != "local" || len(files) != 1 || files[0] != loc {
		t.Fatalf("export kind=%s loc=%s files=%v (want exactly one file, no partials)", kind, loc, files)
	}
	arc := readArchive(t, loc)
	var m struct {
		WorkspaceID string `json:"workspace_id"`
		Files       []struct {
			Name string `json:"name"`
			Rows int64  `json:"rows"`
		} `json:"files"`
	}
	if err := json.Unmarshal([]byte(arc["manifest.json"]), &m); err != nil || m.WorkspaceID != id {
		t.Fatalf("manifest: %v %s", err, arc["manifest.json"])
	}
	if !strings.Contains(arc["audit_log.jsonl"], `"resource.create"`) || !strings.Contains(arc["connector_certificates.jsonl"], `"serial-`) {
		t.Fatalf("archive misses the tenant's audit or certificate rows: %v", m.Files)
	}
	// provider_audit_logs is untouched: earlier rows stay, one tenant.purged row added.
	var after int
	e.scalar(t, &after, `SELECT count(*) FROM provider_audit_logs`)
	if after != providerRowsBefore+1 {
		t.Fatalf("provider audit rows %d → %d, want +1", providerRowsBefore, after)
	}
	if len(e.auditRows(t, provider.ActionTenantPurged)) == 0 {
		t.Fatal("no tenant.purged provider audit row")
	}
}

// The purge exports, verifies, revokes, destroys and tombstones; a crash
// after any step resumes to the same end state with one export.
func TestPurgeDB_CompletesAndResumesAfterCrashAtEachStep(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for i, crashAt := range []string{"", "exported", "revoked", "destroyed", "done"} {
		t.Run("crash="+crashAt, func(t *testing.T) {
			id := e.due(t, fmt.Sprintf("t%d", i))
			var before int
			e.scalar(t, &before, `SELECT count(*) FROM provider_audit_logs`)
			dir := t.TempDir()
			p := e.purger(t, dir)
			if crashAt != "" {
				p.hook = func(ws, step string) error {
					if ws == id && step == crashAt {
						return errors.New("simulated crash")
					}
					return nil
				}
				if err := p.Purge(ctx, id); err == nil || !strings.Contains(err.Error(), "simulated crash") {
					t.Fatalf("crash run: %v", err)
				}
				p = e.purger(t, dir) // restarted controller
			}
			if err := p.RunOnce(ctx); err != nil {
				t.Fatalf("resume: %v", err)
			}
			e.assertPurged(t, id, dir, before)
			// A further pass changes nothing.
			if err := p.RunOnce(ctx); err != nil {
				t.Fatalf("second pass: %v", err)
			}
		})
	}
}

// A legal hold keeps the purge from starting, and pauses one already
// running before its next step; clearing it lets the purge finish.
func TestPurgeDB_LegalHold(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	dir := t.TempDir()
	id := e.due(t, "held")
	if _, _, err := e.h.Store.SetLegalHold(ctx, id, true, "op", "litigation", nil); err != nil {
		t.Fatal(err)
	}
	if err := e.purger(t, dir).RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var n int
	e.scalar(t, &n, `SELECT count(*) FROM tenant_purges WHERE workspace_id = $1`, id)
	if n != 0 {
		t.Fatal("a held tenant's purge started")
	}
	if _, _, err := e.h.Store.SetLegalHold(ctx, id, false, "op", "settled", nil); err != nil {
		t.Fatal(err)
	}
	// Hold placed right after the export: the purge stops there.
	p := e.purger(t, dir)
	p.hook = func(ws, step string) error {
		if step == "exported" {
			if _, _, err := e.h.Store.SetLegalHold(ctx, ws, true, "op", "new matter", nil); err != nil {
				return err
			}
		}
		return nil
	}
	if err := p.Purge(ctx, id); err != nil {
		t.Fatal(err)
	}
	var step string
	e.scalar(t, &step, `SELECT step FROM tenant_purges WHERE workspace_id = $1`, id)
	if step != "exported" {
		t.Fatalf("held purge step = %s, want exported", step)
	}
	// Cancel is refused once the purge started.
	if _, _, err := e.h.Store.CancelDelete(ctx, id, nil); !errors.Is(err, ErrPurgeStarted) {
		t.Fatalf("cancel during purge: %v", err)
	}
	if _, _, err := e.h.Store.SetLegalHold(ctx, id, false, "op", "done", nil); err != nil {
		t.Fatal(err)
	}
	var before int
	e.scalar(t, &before, `SELECT count(*) FROM provider_audit_logs`)
	if err := e.purger(t, dir).RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	e.assertPurged(t, id, dir, before)
}

// Without an export destination nothing is destroyed.
func TestPurgeDB_NoExportDestination(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id := e.due(t, "nodest")
	err := e.purger(t, "").RunOnce(ctx)
	if !errors.Is(err, ErrNoExportDestination) {
		t.Fatalf("err = %v, want ErrNoExportDestination", err)
	}
	var step, lastErr string
	if err := e.pool.QueryRow(ctx, `SELECT step, COALESCE(last_error, '') FROM tenant_purges WHERE workspace_id = $1`, id).Scan(&step, &lastErr); err != nil {
		t.Fatal(err)
	}
	var n int
	e.scalar(t, &n, `SELECT count(*) FROM audit_logs WHERE tenant_id = $1`, id)
	if step != "pending" || lastErr == "" || n == 0 {
		t.Fatalf("step=%s last_error=%q audit rows=%d", step, lastErr, n)
	}
}

// D-12, D-13: the runtime role can't delete workspaces or audit rows; the
// privileged function refuses without a verified export and the revoke
// step, or while not due.
func TestPurgeDB_PrivilegedPath(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id := e.due(t, "priv")
	conn, err := e.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SET ROLE ztna_app`); err != nil {
		t.Fatal(err)
	}
	defer conn.Exec(context.Background(), `RESET ROLE`) //nolint:errcheck
	denied := func(what, q string, args ...any) {
		t.Helper()
		_, err := conn.Exec(ctx, q, args...)
		var pe *pgconn.PgError
		if !errors.As(err, &pe) || pe.Code != "42501" {
			t.Fatalf("%s as ztna_app: err = %v, want insufficient_privilege", what, err)
		}
	}
	denied("delete workspace", `DELETE FROM workspaces WHERE id = $1`, id)
	denied("delete audit_logs", `DELETE FROM audit_logs WHERE tenant_id = $1`, id)
	denied("delete provider_audit_logs", `DELETE FROM provider_audit_logs`)
	denied("delete tombstones", `DELETE FROM workspace_tombstones`)
	denied("tenant_purge without a purge row", `SELECT tenant_purge($1)`, id)
	// The app role can't write purge progress or tombstones directly: only
	// through the step functions, which check their preconditions.
	denied("insert a purge row", `INSERT INTO tenant_purges (workspace_id, slug, delete_by, delete_reason, scheduled_at)
		VALUES ($1, 'priv', 'op', 'r', NOW())`, id)
	denied("insert a tombstone", `INSERT INTO workspace_tombstones (workspace_id, slug, deleted_by, delete_reason, scheduled_at,
		export_kind, export_location, export_sha256) VALUES ($1, 'x', 'x', 'x', NOW(), 'local', '/x', repeat('a', 64))`, id)
	var started bool
	if err := conn.QueryRow(ctx, `SELECT tenant_purge_start($1)`, id).Scan(&started); err != nil || !started {
		t.Fatalf("tenant_purge_start on a due tenant: %v %v", started, err)
	}
	denied("update a purge row", `UPDATE tenant_purges SET step = 'revoked' WHERE workspace_id = $1`, id)
	denied("tenant_purge before the export", `SELECT tenant_purge($1)`, id)
	denied("mark revoked before the export", `SELECT tenant_purge_mark_revoked($1)`, id)
	if _, err := conn.Exec(ctx, `SELECT tenant_purge_record_export($1, 'local', '/x', repeat('a', 64))`, id); err != nil {
		t.Fatalf("record export: %v", err)
	}
	denied("record the export twice", `SELECT tenant_purge_record_export($1, 'local', '/y', repeat('b', 64))`, id)
	denied("mark revoked with an active connector", `SELECT tenant_purge_mark_revoked($1)`, id)
	denied("tenant_purge before the revoke step", `SELECT tenant_purge($1)`, id)
	var n int
	e.scalar(t, &n, `SELECT count(*) FROM workspaces WHERE id = $1`, id)
	if n != 1 {
		t.Fatal("workspace deleted without export")
	}
}

// fakeBucket is an S3 archive target in memory.
type fakeBucket struct {
	objects map[string][]byte
	corrupt bool
}

func (b *fakeBucket) Key(ws, name string) string {
	return "zecurity/v1/" + ws + "/tenant-deletion/" + name
}
func (b *fakeBucket) BucketName() string { return "tenant-bucket" }
func (b *fakeBucket) Put(_ context.Context, _ *http.Client, key, path, _ string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if b.corrupt {
		data = append(data, 'x')
	}
	b.objects[key] = data
	return nil
}
func (b *fakeBucket) SHA256(_ context.Context, _ *http.Client, key string) (string, error) {
	f, err := os.CreateTemp("", "fakebucket-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())
	f.Write(b.objects[key]) //nolint:errcheck
	f.Close()
	return tenantexport.FileSHA256(f.Name())
}

type fakeArchives struct {
	bucket *fakeBucket
	none   bool
}

func (f fakeArchives) TenantS3Archive(context.Context, string) (ArchiveTarget, error) {
	if f.none {
		return nil, auditexport.ErrNoS3Sink
	}
	return f.bucket, nil
}

// A tenant with an S3 sink gets its archive there (not the local dir); a
// read-back mismatch stops the purge before anything is destroyed.
func TestPurgeDB_S3ExportAndVerification(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	dir := t.TempDir()

	bad := e.due(t, "s3bad")
	p := e.purger(t, dir)
	p.S3 = fakeArchives{bucket: &fakeBucket{objects: map[string][]byte{}, corrupt: true}}
	if err := p.Purge(ctx, bad); err == nil || !strings.Contains(err.Error(), "verify S3 export") {
		t.Fatalf("corrupted S3 copy: err = %v", err)
	}
	var step string
	e.scalar(t, &step, `SELECT step FROM tenant_purges WHERE workspace_id = $1`, bad)
	if step != "pending" {
		t.Fatalf("step after a failed verification = %s, want pending", step)
	}

	good := e.due(t, "s3good")
	bucket := &fakeBucket{objects: map[string][]byte{}}
	p = e.purger(t, dir)
	p.S3 = fakeArchives{bucket: bucket}
	if err := p.Purge(ctx, good); err != nil {
		t.Fatal(err)
	}
	var kind, loc string
	if err := e.pool.QueryRow(ctx, `SELECT export_kind, export_location FROM workspace_tombstones WHERE workspace_id = $1`, good).Scan(&kind, &loc); err != nil {
		t.Fatal(err)
	}
	if kind != "s3" || !strings.HasPrefix(loc, "s3://tenant-bucket/zecurity/v1/"+good+"/tenant-deletion/") || len(bucket.objects) != 1 {
		t.Fatalf("s3 export: kind=%s loc=%s objects=%d", kind, loc, len(bucket.objects))
	}
	if files, _ := filepath.Glob(filepath.Join(dir, good, "*")); len(files) != 0 {
		t.Fatalf("a tenant with an S3 sink also got a local export: %v", files)
	}
}

// A local copy that doesn't match its hash stops the purge.
func TestPurgeDB_LocalVerification(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id := e.due(t, "localbad")
	orig := copyFile
	copyFile = func(src, dst string) error {
		if err := orig(src, dst); err != nil {
			return err
		}
		f, err := os.OpenFile(dst, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = f.WriteString("x")
		return err
	}
	defer func() { copyFile = orig }()
	if err := e.purger(t, t.TempDir()).Purge(ctx, id); err == nil || !strings.Contains(err.Error(), "verify local export") {
		t.Fatalf("corrupted local copy: err = %v", err)
	}
	var step string
	e.scalar(t, &step, `SELECT step FROM tenant_purges WHERE workspace_id = $1`, id)
	if step != "pending" {
		t.Fatalf("step = %s, want pending", step)
	}
}

// tenant_purge itself refuses while a legal hold is set or before the grace
// ends, even with a verified export and the revoke step recorded.
func TestPurgeDB_TenantPurgeFunctionChecks(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id := e.due(t, "fnchk")
	if _, err := e.pool.Exec(ctx, `INSERT INTO tenant_purges (workspace_id, slug, step, export_kind, export_location, export_sha256,
		export_verified_at, delete_by, delete_reason, scheduled_at)
		VALUES ($1, 'fnchk', 'revoked', 'local', '/x', repeat('a', 64), NOW(), 'op', 'r', NOW())`, id); err != nil {
		t.Fatal(err)
	}
	call := func() error {
		_, err := e.pool.Exec(ctx, `SELECT tenant_purge($1)`, id)
		return err
	}
	if _, err := e.pool.Exec(ctx, `UPDATE workspaces SET legal_hold = true, legal_hold_reason = 'r', legal_hold_by = 'op', legal_hold_at = NOW() WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if err := call(); err == nil {
		t.Fatal("tenant_purge ran under a legal hold")
	}
	if _, err := e.pool.Exec(ctx, `UPDATE workspaces SET legal_hold = false, legal_hold_reason = NULL, legal_hold_by = NULL, legal_hold_at = NULL,
		delete_after = NOW() + INTERVAL '1 day' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if err := call(); err == nil {
		t.Fatal("tenant_purge ran before the grace ended")
	}
	if _, err := e.pool.Exec(ctx, `UPDATE workspaces SET delete_after = NOW() - INTERVAL '1 second' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if err := call(); err != nil {
		t.Fatalf("tenant_purge when due: %v", err)
	}
	var n int
	e.scalar(t, &n, `SELECT count(*) FROM workspaces WHERE id = $1`, id)
	if n != 0 {
		t.Fatal("workspace not deleted")
	}
}

// The purge row is created only for a due tenant, under the workspace lock:
// after a cancel (or under a legal hold) tenant_purge_start creates none, so
// a cancelled deletion can never go on to revoke the tenant.
func TestPurgeDB_StartOnlyWhenDue(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id := e.due(t, "cancelled")
	if _, _, err := e.h.Store.CancelDelete(ctx, id, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.purger(t, t.TempDir()).Purge(ctx, id); err != nil {
		t.Fatal(err)
	}
	var n int
	e.scalar(t, &n, `SELECT count(*) FROM tenant_purges WHERE workspace_id = $1`, id)
	var active string
	e.scalar(t, &active, `SELECT status FROM connectors WHERE tenant_id = $1`, id)
	if n != 0 || active != "active" {
		t.Fatalf("purge rows=%d connector=%s after a cancelled deletion, want 0 and active", n, active)
	}
}
