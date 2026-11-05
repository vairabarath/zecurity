package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// newChainDB creates a throwaway database from PKI_TEST_DATABASE_URL, applies
// every schema file, seeds one workspace, and returns an owner pool (the
// migration role) and an app pool that runs as ztna_app (D-12, S-5).
func newChainDB(t *testing.T) (owner, app *pgxpool.Pool, workspaceID string) {
	t.Helper()
	adminDSN := os.Getenv("PKI_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("PKI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	dbName := fmt.Sprintf("audit_chain_test_%d_%d", os.Getpid(), time.Now().UnixNano())

	admin, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		admin.Close()
		t.Fatalf("create test database: %v", err)
	}
	parsed, err := url.Parse(adminDSN)
	if err != nil {
		admin.Close()
		t.Fatalf("parse PKI_TEST_DATABASE_URL: %v", err)
	}
	parsed.Path = "/" + dbName

	owner, err = pgxpool.New(ctx, parsed.String())
	if err != nil {
		admin.Close()
		t.Fatalf("connect test database: %v", err)
	}
	appCfg, err := pgxpool.ParseConfig(parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	appCfg.MaxConns = 32
	appCfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		_, err := c.Exec(ctx, "SET ROLE ztna_app")
		return err
	}
	t.Cleanup(func() {
		if app != nil {
			app.Close()
		}
		owner.Close()
		if _, err := admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+dbName+" WITH (FORCE)"); err != nil {
			t.Logf("drop test database: %v", err)
		}
		admin.Close()
	})

	files, err := filepath.Glob(filepath.Join("..", "..", "migrations", "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("list migrations: %v (%d files)", err, len(files))
	}
	sort.Strings(files)
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := owner.Exec(ctx, string(b)); err != nil {
			t.Fatalf("apply %s: %v", filepath.Base(f), err)
		}
	}

	app, err = pgxpool.NewWithConfig(ctx, appCfg)
	if err != nil {
		t.Fatalf("connect app pool: %v", err)
	}
	slug := fmt.Sprintf("chain-%d", time.Now().UnixNano())
	if err := app.QueryRow(ctx,
		`INSERT INTO workspaces (slug, name, status, trust_domain) VALUES ($1, $1, 'active', $2) RETURNING id`,
		slug, "ws-"+slug+".zecurity.in",
	).Scan(&workspaceID); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	return owner, app, workspaceID
}

func insertAccess(ctx context.Context, db interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, ws, msg, action string, createdAt *time.Time) error {
	_, err := db.Exec(ctx,
		`INSERT INTO connector_logs (workspace_id, connector_id, message, action, destination, port, protocol, created_at, occurred_at)
		 VALUES ($1, 'conn-1', $2, $3, '10.0.0.1', 22, 'tcp', COALESCE($4, NOW()), COALESCE($4, NOW()))`,
		ws, msg, action, createdAt)
	return err
}

func mustVerify(t *testing.T, db Querier, ws string, s Stream, wantHead int64) VerifyResult {
	t.Helper()
	res, err := VerifyStream(context.Background(), db, ws, s)
	if err != nil {
		t.Fatalf("verify %s: %v", s, err)
	}
	if !res.OK() {
		t.Fatalf("verify %s: breaks %+v", s, res.Breaks)
	}
	if res.HeadSeq != wantHead {
		t.Fatalf("verify %s: head seq %d, want %d", s, res.HeadSeq, wantHead)
	}
	return res
}

// Concurrent writers to both streams of one workspace produce gap-free,
// correctly linked chains (P-2), through the app role.
func TestChain_ConcurrentAppendsStayContinuous(t *testing.T) {
	owner, app, ws := newChainDB(t)
	ctx := context.Background()
	const workers, per = 8, 25

	var wg sync.WaitGroup
	errs := make(chan error, 2*workers*per)
	for w := 0; w < workers; w++ {
		wg.Add(2)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < per; i++ {
				if err := Record(ctx, app, Entry{
					TenantID: ws, ActorEmail: "a@example.com", Action: "group.create",
					TargetType: "group", TargetID: fmt.Sprintf("g-%d-%d", w, i),
					Details: map[string]any{"w": w, "i": i, "name": "ünïcode ✓"},
				}); err != nil {
					errs <- err
				}
			}
		}(w)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < per; i++ {
				if err := insertAccess(ctx, app, ws, fmt.Sprintf("m-%d-%d", w, i), "allow", nil); err != nil {
					errs <- err
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("append: %v", err)
	}

	mustVerify(t, owner, ws, StreamAdmin, workers*per)
	mustVerify(t, app, ws, StreamAccess, workers*per)
}

// A transaction that locks the workspace row FOR UPDATE and then audits, racing
// plain autocommit Records, must not deadlock: the chain trigger takes the
// workspace KEY SHARE lock before the head lock (lock order workspace, head).
// Also covers the RecordTx-last pattern (member_role, device approval).
func TestChain_LockOrderNoDeadlock(t *testing.T) {
	owner, app, ws := newChainDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	const rounds = 40

	var wg sync.WaitGroup
	errs := make(chan error, 4*rounds)
	for r := 0; r < rounds; r++ {
		wg.Add(2)
		go func(r int) {
			defer wg.Done()
			tx, err := app.Begin(ctx)
			if err != nil {
				errs <- err
				return
			}
			defer tx.Rollback(ctx)
			if _, err := tx.Exec(ctx, `SELECT 1 FROM workspaces WHERE id = $1 FOR UPDATE`, ws); err != nil {
				errs <- err
				return
			}
			time.Sleep(time.Millisecond)
			if err := RecordTx(ctx, tx, Entry{
				TenantID: ws, ActorEmail: "tx@example.com", Action: "member.role_changed",
				TargetType: "user", TargetID: strconv.Itoa(r),
			}); err != nil {
				errs <- err
				return
			}
			if err := tx.Commit(ctx); err != nil {
				errs <- err
			}
		}(r)
		go func(r int) {
			defer wg.Done()
			if err := Record(ctx, app, Entry{
				TenantID: ws, ActorEmail: "plain@example.com", Action: "group.update",
				TargetType: "group", TargetID: strconv.Itoa(r),
			}); err != nil {
				errs <- err
			}
			if err := insertAccess(ctx, app, ws, "x", "deny", nil); err != nil {
				errs <- err
			}
		}(r)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "40P01" {
			t.Fatalf("deadlock: %v", err)
		}
		t.Fatalf("append: %v", err)
	}
	mustVerify(t, owner, ws, StreamAdmin, 2*rounds)
	mustVerify(t, owner, ws, StreamAccess, rounds)
}

// D-12: the app role can insert and read the audit tables but never change or
// remove rows, write stream heads, or call the chain's internal functions.
func TestD12_AppRoleIsAppendOnly(t *testing.T) {
	_, app, ws := newChainDB(t)
	ctx := context.Background()
	if err := Record(ctx, app, Entry{TenantID: ws, ActorEmail: "a@example.com", Action: "x.y", TargetType: "t", TargetID: "1"}); err != nil {
		t.Fatalf("app insert: %v", err)
	}
	if err := insertAccess(ctx, app, ws, "m", "allow", nil); err != nil {
		t.Fatalf("app insert access: %v", err)
	}

	refused := []string{
		`UPDATE audit_logs SET actor_email = 'evil'`,
		`DELETE FROM audit_logs`,
		`TRUNCATE audit_logs`,
		`UPDATE connector_logs SET action = 'allow'`,
		`DELETE FROM connector_logs`,
		`TRUNCATE connector_logs`,
		`UPDATE provider_audit_logs SET action = 'x'`,
		`DELETE FROM provider_audit_logs`,
		`UPDATE audit_stream_heads SET last_seq = 0`,
		`DELETE FROM audit_stream_heads`,
		`INSERT INTO audit_stream_heads (workspace_id, stream, last_hash) VALUES ('` + ws + `', 'admin', '\x00')`,
		`SELECT audit_chain_advance('` + ws + `', 'admin', 0, '\x00')`,
		`SELECT * FROM audit_chain_lock('` + ws + `', 'admin')`,
		`RESET ROLE`, // sanity: proves the pool really is ztna_app (owner could reset)
	}
	for _, q := range refused[:len(refused)-1] {
		_, err := app.Exec(ctx, q)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Errorf("%s: want permission denied (42501), got %v", q, err)
		}
	}
	var role string
	if err := app.QueryRow(ctx, `SELECT current_user`).Scan(&role); err != nil || role != "ztna_app" {
		t.Fatalf("app pool runs as %q (%v), want ztna_app", role, err)
	}
	// Retention is the one sanctioned delete path, and the app may call it.
	if _, err := app.Exec(ctx, `SELECT audit_purge_access(NOW() - INTERVAL '90 days', 10)`); err != nil {
		t.Fatalf("app purge call: %v", err)
	}
}

// An edit by the table owner (who bypasses D-12) is caught by the chain, and
// so is a deleted row.
func TestChain_DetectsOwnerTampering(t *testing.T) {
	owner, app, ws := newChainDB(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if err := Record(ctx, app, Entry{TenantID: ws, ActorEmail: "a@example.com", Action: "group.create", TargetType: "group", TargetID: strconv.Itoa(i)}); err != nil {
			t.Fatal(err)
		}
	}
	mustVerify(t, owner, ws, StreamAdmin, 5)

	if _, err := owner.Exec(ctx, `UPDATE audit_logs SET actor_email = 'someone-else@example.com' WHERE tenant_id = $1 AND seq = 3`, ws); err != nil {
		t.Fatal(err)
	}
	res, err := VerifyStream(ctx, owner, ws, StreamAdmin)
	if err != nil {
		t.Fatal(err)
	}
	if res.OK() || res.Breaks[0].Seq != 3 || !strings.Contains(res.Breaks[0].Reason, "row edited") {
		t.Fatalf("edit not detected at seq 3: %+v", res.Breaks)
	}

	if _, err := owner.Exec(ctx, `DELETE FROM audit_logs WHERE tenant_id = $1 AND seq = 5`, ws); err != nil {
		t.Fatal(err)
	}
	res, err = VerifyStream(ctx, owner, ws, StreamAdmin)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, b := range res.Breaks {
		if strings.Contains(b.Reason, "head is at 5") {
			found = true
		}
	}
	if !found {
		t.Fatalf("deleted last row not detected: %+v", res.Breaks)
	}
}

// Retention deletes only a prefix of old rows, in batches, records the
// checkpoint, and the remaining stream still verifies from it (P-4, S-6).
func TestPurgeAccess_PrefixAndCheckpoint(t *testing.T) {
	owner, app, ws := newChainDB(t)
	ctx := context.Background()
	old := time.Now().Add(-100 * 24 * time.Hour)
	for i := 0; i < 7; i++ {
		if err := insertAccess(ctx, app, ws, "old", "allow", &old); err != nil {
			t.Fatal(err)
		}
	}
	if err := insertAccess(ctx, app, ws, "new", "allow", nil); err != nil {
		t.Fatal(err)
	}
	// An old row after a new one is not purged: purge stops at the first new row.
	if err := insertAccess(ctx, app, ws, "late-old", "allow", &old); err != nil {
		t.Fatal(err)
	}

	purge := func(batch int) int64 {
		var n int64
		if err := app.QueryRow(ctx, `SELECT audit_purge_access(NOW() - INTERVAL '90 days', $1)`, batch).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := purge(3); n != 3 {
		t.Fatalf("first batch purged %d, want 3", n)
	}
	mustVerify(t, owner, ws, StreamAccess, 9)
	if n := purge(100); n != 4 {
		t.Fatalf("second batch purged %d, want 4", n)
	}
	if n := purge(100); n != 0 {
		t.Fatalf("third batch purged %d, want 0", n)
	}
	res := mustVerify(t, owner, ws, StreamAccess, 9)
	if res.FromSeq != 8 || res.RowsChecked != 2 {
		t.Fatalf("after purge: from %d rows %d, want from 8 rows 2", res.FromSeq, res.RowsChecked)
	}
	var purgedSeq int64
	if err := owner.QueryRow(ctx, `SELECT purged_through_seq FROM audit_stream_heads WHERE workspace_id = $1 AND stream = 'access'`, ws).Scan(&purgedSeq); err != nil || purgedSeq != 7 {
		t.Fatalf("purged_through_seq = %d (%v), want 7", purgedSeq, err)
	}
}

// The hashed bytes don't depend on session settings (TimeZone, DateStyle):
// rows written under odd settings still verify under the defaults.
func TestChain_CanonicalIgnoresSessionSettings(t *testing.T) {
	owner, app, ws := newChainDB(t)
	ctx := context.Background()
	conn, err := app.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `SET TimeZone = 'Asia/Kolkata'; SET DateStyle = 'SQL, DMY'; SET extra_float_digits = 0`); err != nil {
		t.Fatal(err)
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := RecordTx(ctx, tx, Entry{TenantID: ws, ActorEmail: "a@example.com", Action: "x.y", TargetType: "t", TargetID: "1", Details: map[string]any{"f": 1.5}}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	conn.Release()
	mustVerify(t, owner, ws, StreamAdmin, 1)
}

// The canonical encoding is reproducible outside Postgres from the exported
// fields: a Go encoder over the row's values yields the database's bytes.
func TestChain_CanonicalMatchesGoEncoding(t *testing.T) {
	owner, app, ws := newChainDB(t)
	ctx := context.Background()
	if err := Record(ctx, app, Entry{TenantID: ws, ActorEmail: "ä@example.com", Action: "group.create", TargetType: "group", TargetID: "g1", Details: map[string]any{"name": "x", "a": []int{1, 2}}}); err != nil {
		t.Fatal(err)
	}
	var (
		id, email, action, ttype, tid string
		actor                         *string
		details                       string
		createdMicros                 int64
		dbCanonical                   []byte
	)
	if err := owner.QueryRow(ctx,
		`SELECT id::text, actor_user_id::text, actor_email, action, target_type, target_id, details::text,
		        (extract(epoch FROM created_at) * 1000000)::bigint, audit_admin_canonical(a)
		   FROM audit_logs a WHERE tenant_id = $1 AND seq = 1`, ws,
	).Scan(&id, &actor, &email, &action, &ttype, &tid, &details, &createdMicros, &dbCanonical); err != nil {
		t.Fatal(err)
	}
	field := func(v *string) string {
		if v == nil {
			return "-"
		}
		return strconv.Itoa(len(*v)) + ":" + *v
	}
	s := func(v string) *string { return &v }
	goCanonical := field(s("v1")) + field(s("admin")) + field(s("1")) + field(&id) + field(&ws) +
		field(actor) + field(&email) + field(&action) + field(&ttype) + field(&tid) + field(&details) +
		field(s(strconv.FormatInt(createdMicros, 10)))
	if goCanonical != string(dbCanonical) {
		t.Fatalf("canonical mismatch:\n go %q\n db %q", goCanonical, dbCanonical)
	}
	if !json.Valid([]byte(details)) {
		t.Fatalf("details_canonical is not valid JSON: %q", details)
	}
}
