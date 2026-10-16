package idp

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourorg/ztna/controller/internal/outbox"
	"github.com/yourorg/ztna/controller/internal/policy"
)

// Certificate audit bugs 4/5: disabling an identity connection committed the
// connection status first and then, best-effort and separately, suspended its
// SCIM users — with no policy invalidation at all. Deleting a connection
// notified the policy best-effort after commit. The lifecycle change and its
// durable policy-invalidation work must now commit together, or not at all.

type lifecycleTxFixture struct {
	pool   *pgxpool.Pool
	ctx    context.Context
	store  *Store
	ob     *outbox.Outbox
	ws     string
	connID string
	scim   string // SCIM-managed user linked to the connection
	local  string // non-SCIM user linked to the connection
}

func newLifecycleTxFixture(t *testing.T) *lifecycleTxFixture {
	t.Helper()
	adminDSN := os.Getenv("PKI_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("PKI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	dbName := fmt.Sprintf("idp_lifecycle_%d_%d", os.Getpid(), time.Now().UnixNano())
	adminPool := mustConnectPool(t, ctx, adminDSN)
	t.Cleanup(adminPool.Close)
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	t.Cleanup(func() { _, _ = adminPool.Exec(context.Background(), "DROP DATABASE IF EXISTS "+dbName) })
	dsn, err := withDBName(adminDSN, dbName)
	if err != nil {
		t.Fatalf("dsn: %v", err)
	}
	pool := mustConnectPool(t, ctx, dsn)
	t.Cleanup(pool.Close)
	if err := applyMigrations(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	store := NewStore(pool, base64Enc{})
	ws := mustInsertWorkspace(t, ctx, pool, "ws-lifecycle-tx")
	conn, err := store.CreateWorkspaceConnection(ctx, ws, CreateInput{
		Provider: "okta", DisplayName: "Acme Okta", Issuer: "https://acme.okta.com",
		ClientID: "client-1", ClientSecret: "secret-1",
	})
	if err != nil {
		t.Fatalf("create connection: %v", err)
	}
	f := &lifecycleTxFixture{pool: pool, ctx: ctx, store: store, ob: outbox.NewOutbox(pool), ws: ws, connID: conn.ID}
	f.scim = f.linkedUser(t, "scim")
	f.local = f.linkedUser(t, "unmanaged")
	return f
}

func (f *lifecycleTxFixture) linkedUser(t *testing.T, owner string) string {
	t.Helper()
	var id string
	if err := f.pool.QueryRow(f.ctx,
		`INSERT INTO users (tenant_id, email, provider, provider_sub, role, status, provisioning_owner)
		 VALUES ($1, gen_random_uuid()::text || '@acme.test', 'okta', gen_random_uuid()::text, 'member', 'active', $2)
		 RETURNING id::text`, f.ws, owner).Scan(&id); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO external_identities (tenant_id, user_id, connection_id, issuer, subject)
		 VALUES ($1, $2, $3, 'https://acme.okta.com', gen_random_uuid()::text)`, f.ws, id, f.connID); err != nil {
		t.Fatalf("link user: %v", err)
	}
	return id
}

// invalidate is the in-transaction hook production uses: durable policy
// invalidation for the workspace.
func (f *lifecycleTxFixture) invalidate(reason string) InTx {
	return func(ctx context.Context, tx pgx.Tx) error {
		return f.ob.Enqueue(ctx, tx, policy.NewInvalidationEvent(uuid.MustParse(f.ws), reason))
	}
}

func (f *lifecycleTxFixture) scalar(t *testing.T, sql string, args ...any) string {
	t.Helper()
	var v string
	if err := f.pool.QueryRow(f.ctx, sql, args...).Scan(&v); err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	return v
}

func (f *lifecycleTxFixture) invalidations(t *testing.T) string {
	return f.scalar(t, `SELECT count(*)::text FROM outbox_events WHERE event_type = $1 AND workspace_id = $2`,
		policy.EventPolicyInvalidationRequested, f.ws)
}

func (f *lifecycleTxFixture) failOutboxInserts(t *testing.T) {
	t.Helper()
	for _, sql := range []string{
		`CREATE FUNCTION fail_outbox() RETURNS trigger AS $$ BEGIN RAISE EXCEPTION 'outbox unavailable'; END; $$ LANGUAGE plpgsql`,
		`CREATE TRIGGER fail_outbox BEFORE INSERT ON outbox_events FOR EACH ROW EXECUTE FUNCTION fail_outbox()`,
	} {
		if _, err := f.pool.Exec(f.ctx, sql); err != nil {
			t.Fatalf("install failure trigger: %v", err)
		}
	}
}

func TestDisableConnection_SuspendsAndRecordsInvalidationAtomically(t *testing.T) {
	f := newLifecycleTxFixture(t)
	suspended, _, err := f.store.DisableConnection(f.ctx, f.ws, f.connID, f.invalidate("idp.connection.disable"))
	if err != nil {
		t.Fatalf("disable: %v", err)
	}
	if suspended != 1 {
		t.Fatalf("suspended %d users, want 1 (the SCIM-managed one)", suspended)
	}
	if got := f.scalar(t, `SELECT status FROM identity_connections WHERE id = $1`, f.connID); got != "disabled" {
		t.Fatalf("connection status = %s", got)
	}
	if got := f.scalar(t, `SELECT status FROM users WHERE id = $1`, f.scim); got != "suspended" {
		t.Fatalf("SCIM user status = %s", got)
	}
	if got := f.scalar(t, `SELECT provisioning_owner FROM users WHERE id = $1`, f.scim); got != "unmanaged" {
		t.Fatalf("SCIM user owner = %s, want unmanaged (ADR-025 §12)", got)
	}
	if got := f.scalar(t, `SELECT status FROM users WHERE id = $1`, f.local); got != "active" {
		t.Fatalf("non-SCIM user must not be suspended, got %s", got)
	}
	if got := f.invalidations(t); got != "1" {
		t.Fatalf("policy invalidation events = %s, want 1", got)
	}
}

func TestDisableConnection_RollsBackWhenInvalidationCannotBeRecorded(t *testing.T) {
	f := newLifecycleTxFixture(t)
	f.failOutboxInserts(t)
	if _, _, err := f.store.DisableConnection(f.ctx, f.ws, f.connID, f.invalidate("idp.connection.disable")); err == nil {
		t.Fatal("disable succeeded although its policy invalidation could not be recorded")
	}
	if got := f.scalar(t, `SELECT status FROM identity_connections WHERE id = $1`, f.connID); got != "active" {
		t.Fatalf("connection status = %s: committed without its invalidation", got)
	}
	if got := f.scalar(t, `SELECT status FROM users WHERE id = $1`, f.scim); got != "active" {
		t.Fatalf("SCIM user status = %s: committed without its invalidation", got)
	}
	if got := f.scalar(t, `SELECT provisioning_owner FROM users WHERE id = $1`, f.scim); got != "scim" {
		t.Fatalf("SCIM user owner = %s: committed without its invalidation", got)
	}
}

func TestDeleteConnection_RecordsInvalidationAtomically(t *testing.T) {
	t.Run("soft delete", func(t *testing.T) {
		f := newLifecycleTxFixture(t)
		if err := f.store.SoftDeleteConnectionTx(f.ctx, f.ws, f.connID, f.invalidate("idp.connection.delete")); err != nil {
			t.Fatalf("soft delete: %v", err)
		}
		if got := f.invalidations(t); got != "1" {
			t.Fatalf("policy invalidation events = %s, want 1", got)
		}
	})
	t.Run("soft delete rolls back without its invalidation", func(t *testing.T) {
		f := newLifecycleTxFixture(t)
		f.failOutboxInserts(t)
		if err := f.store.SoftDeleteConnectionTx(f.ctx, f.ws, f.connID, f.invalidate("idp.connection.delete")); err == nil {
			t.Fatal("soft delete succeeded although its invalidation could not be recorded")
		}
		if got := f.scalar(t, `SELECT status FROM identity_connections WHERE id = $1`, f.connID); got != "active" {
			t.Fatalf("connection status = %s: committed without its invalidation", got)
		}
	})
	t.Run("hard delete", func(t *testing.T) {
		f := newLifecycleTxFixture(t)
		if _, err := f.pool.Exec(f.ctx, `DELETE FROM external_identities WHERE connection_id = $1`, f.connID); err != nil {
			t.Fatalf("unlink users: %v", err)
		}
		if err := f.store.DeleteWorkspaceConnectionTx(f.ctx, f.ws, f.connID, f.invalidate("idp.connection.delete")); err != nil {
			t.Fatalf("hard delete: %v", err)
		}
		if got := f.invalidations(t); got != "1" {
			t.Fatalf("policy invalidation events = %s, want 1", got)
		}
	})
	t.Run("hard delete rolls back without its invalidation", func(t *testing.T) {
		f := newLifecycleTxFixture(t)
		if _, err := f.pool.Exec(f.ctx, `DELETE FROM external_identities WHERE connection_id = $1`, f.connID); err != nil {
			t.Fatalf("unlink users: %v", err)
		}
		f.failOutboxInserts(t)
		if err := f.store.DeleteWorkspaceConnectionTx(f.ctx, f.ws, f.connID, f.invalidate("idp.connection.delete")); err == nil {
			t.Fatal("hard delete succeeded although its invalidation could not be recorded")
		}
		if got := f.scalar(t, `SELECT count(*)::text FROM identity_connections WHERE id = $1`, f.connID); got != "1" {
			t.Fatal("connection deleted without its invalidation")
		}
	})
}
