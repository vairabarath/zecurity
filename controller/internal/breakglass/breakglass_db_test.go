package breakglass_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourorg/ztna/controller/internal/alerting"
	"github.com/yourorg/ztna/controller/internal/breakglass"
	"github.com/yourorg/ztna/controller/internal/provider"
)

// Sprint 27 G1 (D-75): recover-admin needs a reason; every use leaves one
// event that raises one critical alert, one email per active super-admin and
// a banner until acknowledged with a note. Never the temporary password.

const ruleBreakGlass = "00000000-0000-0000-0002-000000000011"

func newDB(t *testing.T) (*pgxpool.Pool, *provider.Store) {
	t.Helper()
	adminDSN := os.Getenv("PKI_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("PKI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	name := fmt.Sprintf("breakglass_test_%d_%d", os.Getpid(), time.Now().UnixNano())
	admin, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		admin.Close()
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
		_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name)
		admin.Close()
	})
	files, _ := filepath.Glob(filepath.Join("..", "..", "migrations", "*.sql"))
	sort.Strings(files)
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, string(b)); err != nil {
			t.Fatalf("apply %s: %v", filepath.Base(f), err)
		}
	}
	return pool, provider.NewStore(pool)
}

type fakeSender struct {
	mu   sync.Mutex
	fail bool
	sent []string // "to|subject|body"
}

func (f *fakeSender) SendPlain(to, subject, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return errors.New("smtp down")
	}
	f.sent = append(f.sent, to+"|"+subject+"|"+body)
	return nil
}

func one[T any](t *testing.T, pool *pgxpool.Pool, q string, args ...any) T {
	t.Helper()
	var v T
	if err := pool.QueryRow(context.Background(), q, args...).Scan(&v); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return v
}

// seed: two active super-admins, one disabled super-admin, one relay-ops.
func seed(t *testing.T, store *provider.Store) (admin, second *provider.ProviderUser) {
	t.Helper()
	ctx := context.Background()
	_, admin, err := provider.Bootstrap(ctx, store, "boot@inkyank.com", "boot-temp-password-1")
	if err != nil {
		t.Fatal(err)
	}
	actor := provider.AuditActor{UserID: admin.ID, Email: admin.Email}
	if second, err = store.CreateOperator(ctx, actor, "second@inkyank.com", provider.RoleSuperAdmin, "x-hash"); err != nil {
		t.Fatal(err)
	}
	gone, err := store.CreateOperator(ctx, actor, "gone@inkyank.com", provider.RoleSuperAdmin, "x-hash")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetDisabled(ctx, actor, gone.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateOperator(ctx, actor, "ops@inkyank.com", provider.RoleRelayOps, "x-hash"); err != nil {
		t.Fatal(err)
	}
	return admin, second
}

func TestRecoverAdmin_ReasonRequired(t *testing.T) {
	pool, store := newDB(t)
	_, second := seed(t, store)
	ctx := context.Background()
	hashBefore := one[string](t, pool, `SELECT password_hash FROM provider_users WHERE id = $1`, second.ID)
	for _, reason := range []string{"", "   ", strings.Repeat("x", 501), "bad\x00reason"} {
		if _, err := provider.RecoverAdmin(ctx, store, second.Email, reason, "ctl-1"); !errors.Is(err, provider.ErrBreakGlassReason) {
			t.Errorf("reason %q: %v, want ErrBreakGlassReason", reason, err)
		}
	}
	if h := one[string](t, pool, `SELECT password_hash FROM provider_users WHERE id = $1`, second.ID); h != hashBefore {
		t.Fatal("a refused recover-admin changed the password")
	}
	if n := one[int](t, pool, `SELECT count(*) FROM provider_break_glass_events`); n != 0 {
		t.Fatalf("events after refused runs: %d", n)
	}
	if n := one[int](t, pool, `SELECT count(*) FROM provider_audit_logs WHERE action = $1`, provider.AuditRecoverAdmin); n != 0 {
		t.Fatalf("audit rows after refused runs: %d", n)
	}
}

func TestBreakGlass_EndToEnd(t *testing.T) {
	pool, store := newDB(t)
	admin, second := seed(t, store)
	ctx := context.Background()
	// Only the break-glass rule runs in this test's evaluator.
	if _, err := pool.Exec(ctx, `UPDATE alert_rules SET enabled = (id = $1)`, ruleBreakGlass); err != nil {
		t.Fatal(err)
	}

	temp, err := provider.RecoverAdmin(ctx, store, second.Email, "lost phone with the authenticator", "ctl-host-1")
	if err != nil || temp == "" {
		t.Fatalf("recover-admin: %v", err)
	}

	// One event, one audit row with the reason and host; no password anywhere.
	eventID := one[string](t, pool, `SELECT id::text FROM provider_break_glass_events`)
	details := one[string](t, pool, `SELECT details::text FROM provider_audit_logs WHERE action = $1`, provider.AuditRecoverAdmin)
	if !strings.Contains(details, "lost phone with the authenticator") || !strings.Contains(details, "ctl-host-1") || !strings.Contains(details, eventID) {
		t.Fatalf("recover_admin audit details: %s", details)
	}
	if _, err := pool.Exec(ctx, `UPDATE provider_break_glass_events SET reason = 'edited'`); err == nil {
		t.Fatal("an event's reason was editable")
	}

	// Alert: one critical platform event from the evaluator.
	ev := &alerting.Evaluator{Store: alerting.NewStore(pool), Observer: alerting.Sources{DB: pool}}
	fired := ev.Tick(ctx)
	if len(fired) != 1 || fired[0].Kind != alerting.KindBreakGlassUsed || fired[0].Severity != alerting.SeverityCritical ||
		fired[0].Scope != alerting.ScopePlatform || fired[0].Transition != alerting.TransitionFired || fired[0].TargetID != eventID {
		t.Fatalf("fired: %+v", fired)
	}
	payload := string(alerting.NewPayload(fired[0], "https://console.test").JSON())
	if !strings.Contains(payload, second.Email) || !strings.Contains(payload, "lost phone") || strings.Contains(payload, temp) {
		t.Fatalf("payload (must name the operator and reason, never the password): %s", payload)
	}
	if again := ev.Tick(ctx); len(again) != 0 {
		t.Fatalf("second tick re-fired: %+v", again)
	}

	// Email: a failing sender marks nothing; then one email per active
	// super-admin (not the disabled one, not relay-ops); never resent.
	sender := &fakeSender{fail: true}
	job := &breakglass.Job{DB: pool, Store: store, Sender: sender}
	if n, err := job.Check(ctx); err != nil || n != 0 {
		t.Fatalf("failing sender: %d %v", n, err)
	}
	if one[*time.Time](t, pool, `SELECT notified_at FROM provider_break_glass_events`) != nil {
		t.Fatal("marked notified although every send failed")
	}
	sender.fail = false
	if n, err := job.Check(ctx); err != nil || n != 1 {
		t.Fatalf("check: %d %v", n, err)
	}
	if len(sender.sent) != 2 || !strings.HasPrefix(sender.sent[0], admin.Email+"|"+breakglass.Subject) || !strings.HasPrefix(sender.sent[1], second.Email+"|") {
		t.Fatalf("sent: %q", sender.sent)
	}
	for _, m := range sender.sent {
		if !strings.Contains(m, second.Email) || !strings.Contains(m, "ctl-host-1") || !strings.Contains(m, "lost phone") || strings.Contains(m, temp) {
			t.Fatalf("email body: %q", m)
		}
	}
	if n, _ := job.Check(ctx); n != 0 || len(sender.sent) != 2 {
		t.Fatalf("resent: %d %d", n, len(sender.sent))
	}
	notified := one[string](t, pool, `SELECT details::text FROM provider_audit_logs WHERE action = $1`, provider.ActionBreakGlassNotified)
	if !strings.Contains(notified, `"recipients": 2`) || strings.Contains(notified, "@") {
		t.Fatalf("notified audit (count, never addresses): %s", notified)
	}

	// Banner routes.
	h := &breakglass.Handler{DB: pool, Store: store, Authz: provider.NewAuthz()}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /provider/break-glass", h.List)
	mux.HandleFunc("POST /provider/break-glass/{id}/ack", h.Ack)
	do := func(role, method, path string, body any) (int, string) {
		var raw []byte
		if body != nil {
			raw, _ = json.Marshal(body)
		}
		req := httptest.NewRequest(method, path, bytes.NewReader(raw))
		actor := provider.Actor{UserID: admin.ID, Email: admin.Email, Role: role}
		req = req.WithContext(provider.WithActor(req.Context(), actor))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	ack := "/provider/break-glass/" + eventID + "/ack"
	for _, role := range []string{provider.RoleRelayOps, provider.RoleAuditor, provider.RoleBilling, provider.RoleSupport, ""} {
		if code, _ := do(role, "GET", "/provider/break-glass", nil); code != 403 {
			t.Errorf("%q list: %d", role, code)
		}
		if code, _ := do(role, "POST", ack, map[string]string{"note": "x"}); code != 403 {
			t.Errorf("%q ack: %d", role, code)
		}
	}
	code, body := do(provider.RoleSuperAdmin, "GET", "/provider/break-glass", nil)
	if code != 200 || !strings.Contains(body, eventID) || !strings.Contains(body, `"notified_count":2`) || strings.Contains(body, temp) {
		t.Fatalf("list: %d %s", code, body)
	}
	for _, note := range []string{"", "   "} {
		if code, body := do(provider.RoleSuperAdmin, "POST", ack, map[string]string{"note": note}); code != 400 || !strings.Contains(body, "note_required") {
			t.Fatalf("ack note %q: %d %s", note, code, body)
		}
	}
	if code, body := do(provider.RoleSuperAdmin, "POST", ack, map[string]string{"note": strings.Repeat("n", 501)}); code != 400 || !strings.Contains(body, "note_too_long") {
		t.Fatalf("long note: %d %s", code, body)
	}
	if code, body := do(provider.RoleSuperAdmin, "POST", ack, map[string]string{"note": "confirmed with Barath by phone"}); code != 204 {
		t.Fatalf("ack: %d %s", code, body)
	}
	if code, _ := do(provider.RoleSuperAdmin, "POST", ack, map[string]string{"note": "again"}); code != 409 {
		t.Fatalf("second ack: %d", code)
	}
	if code, _ := do(provider.RoleSuperAdmin, "POST", "/provider/break-glass/00000000-0000-0000-0000-000000000001/ack", map[string]string{"note": "x"}); code != 404 {
		t.Fatalf("unknown ack: %d", code)
	}
	if code, body := do(provider.RoleSuperAdmin, "GET", "/provider/break-glass", nil); code != 200 || strings.Contains(body, eventID) {
		t.Fatalf("list after ack: %d %s", code, body)
	}
	ackAudit := one[string](t, pool, `SELECT provider_email || ' ' || details::text FROM provider_audit_logs WHERE action = $1`, provider.ActionBreakGlassAcknowledge)
	if !strings.HasPrefix(ackAudit, admin.Email) || !strings.Contains(ackAudit, "confirmed with Barath by phone") {
		t.Fatalf("ack audit: %s", ackAudit)
	}

	// Acknowledged → the alert resolves on the next tick.
	resolved := ev.Tick(ctx)
	if len(resolved) != 1 || resolved[0].Transition != alerting.TransitionResolved || resolved[0].TargetID != eventID {
		t.Fatalf("resolve: %+v", resolved)
	}
}
