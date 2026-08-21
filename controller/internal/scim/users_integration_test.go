package scim

// White-box test package (scim) so it can exercise unexported types
// (provisionResult, userPatch, scope) and resolveScope directly. Follows the
// PKI_TEST_DATABASE_URL + applyAllMigrations harness used by internal/identity.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourorg/ztna/controller/internal/idp"
	"github.com/yourorg/ztna/controller/internal/identity"
	"github.com/yourorg/ztna/controller/internal/policy"
)

func TestDirectoryService_Integration(t *testing.T) {
	adminDSN := os.Getenv("PKI_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("PKI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	dbName := fmt.Sprintf("scim_phase5_test_%d", os.Getpid())
	adminPool := mustConnectPool(t, ctx, adminDSN)
	defer adminPool.Close()
	_, _ = adminPool.Exec(ctx, "DROP DATABASE IF EXISTS "+dbName)
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	defer func() { _, _ = adminPool.Exec(ctx, "DROP DATABASE IF EXISTS "+dbName) }()

	testDSN, err := withDBName(adminDSN, dbName)
	if err != nil {
		t.Fatalf("build test dsn: %v", err)
	}
	pool := mustConnectPool(t, ctx, testDSN)
	defer pool.Close()
	if err := applyAllMigrations(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	// Seed two workspaces, each with one SCIM-enabled connection.
	wsA := seedWorkspace(ctx, t, pool, "alpha")
	wsB := seedWorkspace(ctx, t, pool, "beta")
	connA := seedSCIMConnection(ctx, t, pool, wsA, "okta", "sub", "externalId")
	connB := seedSCIMConnection(ctx, t, pool, wsB, "okta", "sub", "externalId")

	idpStore := idp.NewStore(pool, nil)
	ds := NewDirectoryService(pool, idpStore, identity.NewAuditSink(pool), policy.NewNotifier(policy.NewSnapshotCache()))

	scA := scopeFor(t, ctx, ds, wsA, connA)
	scB := scopeFor(t, ctx, ds, wsB, connB)

	t.Run("provision creates a SCIM-owned canonical user", func(t *testing.T) {
		res, serr := ds.Provision(ctx, scA, map[string]any{
			"userName":    "alice@alpha.example.com",
			"externalId":  "okta-alice-1",
			"displayName": "Alice",
			"active":      true,
		}, "")
		if serr != nil {
			t.Fatalf("provision: %v", serr)
		}
		if !res.created {
			t.Fatal("expected a newly created user")
		}
		if !res.user.Active {
			t.Fatal("expected active user")
		}
		if res.user.UserName != "alice@alpha.example.com" {
			t.Fatalf("unexpected userName: %q", res.user.UserName)
		}
		var provBy, owner, status string
		if err := pool.QueryRow(ctx,
			`SELECT provisioned_by, provisioning_owner, status FROM users WHERE id = $1`,
			res.user.ID,
		).Scan(&provBy, &owner, &status); err != nil {
			t.Fatalf("read user: %v", err)
		}
		if provBy != "scim" || owner != "scim" {
			t.Fatalf("expected scim provenance, got provisioned_by=%q owner=%q", provBy, owner)
		}
		if status != "active" {
			t.Fatalf("expected active status, got %q", status)
		}
	})

	t.Run("re-provision of same canonical key is idempotent (200, not 201)", func(t *testing.T) {
		res, serr := ds.Provision(ctx, scA, map[string]any{
			"userName":   "alice@alpha.example.com",
			"externalId": "okta-alice-1",
		}, "")
		if serr != nil {
			t.Fatalf("re-provision: %v", serr)
		}
		if res.created {
			t.Fatal("expected idempotent re-provision, not a new user")
		}
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM external_identities
			  WHERE connection_id = $1 AND subject = $2`,
			connA, "okta-alice-1",
		).Scan(&n); err != nil {
			t.Fatalf("count links: %v", err)
		}
		if n != 1 {
			t.Fatalf("expected exactly 1 external_identity link, got %d", n)
		}
	})

	t.Run("provisioning a JIT-owned key returns identity_conflict (409)", func(t *testing.T) {
		jitUser := uuid.NewString()
		if _, err := pool.Exec(ctx,
			`INSERT INTO users (id, tenant_id, email, provider, provider_sub, role, status, provisioned_by, provisioning_owner)
			 VALUES ($1,$2,$3,$4,$5,'member','active','jit','jit')`,
			jitUser, wsA, "bob@alpha.example.com", "okta", "okta-bob-1",
		); err != nil {
			t.Fatalf("seed jit user: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO external_identities (tenant_id, user_id, connection_id, issuer, subject)
			 VALUES ($1,$2,$3,$4,$5)`,
			wsA, jitUser, connA, "https://okta.example.com", "okta-bob-1",
		); err != nil {
			t.Fatalf("seed jit link: %v", err)
		}
		res, serr := ds.Provision(ctx, scA, map[string]any{
			"userName":   "bob@alpha.example.com",
			"externalId": "okta-bob-1",
		}, "")
		if res != nil && !res.conflict {
			t.Fatal("expected a conflict result")
		}
		if serr == nil || serr.Status != 409 || serr.ScimType != "identity_conflict" {
			t.Fatalf("expected 409 identity_conflict, got %+v", serr)
		}
		var owner string
		_ = pool.QueryRow(ctx, `SELECT provisioning_owner FROM users WHERE id = $1`, jitUser).Scan(&owner)
		if owner != "jit" {
			t.Fatalf("JIT owner must be untouched, got %q", owner)
		}
	})

	t.Run("update changes directory-owned email and active, maps active=false to suspended", func(t *testing.T) {
		res, serr := ds.Provision(ctx, scA, map[string]any{
			"userName":   "carol@alpha.example.com",
			"externalId": "okta-carol-1",
		}, "")
		if serr != nil {
			t.Fatalf("provision carol: %v", serr)
		}
		active := false
		u, serr := ds.Update(ctx, scA, res.user.ID, &userPatch{Email: "carol.new@alpha.example.com", Active: &active}, "")
		if serr != nil {
			t.Fatalf("update carol: %v", serr)
		}
		if u.Active {
			t.Fatal("expected user to be suspended (active=false)")
		}
		var status string
		_ = pool.QueryRow(ctx, `SELECT status FROM users WHERE id = $1`, res.user.ID).Scan(&status)
		if status != "suspended" {
			t.Fatalf("expected suspended, got %q", status)
		}
	})

	t.Run("update rejects unowned directory attributes", func(t *testing.T) {
		res, serr := ds.Provision(ctx, scA, map[string]any{
			"userName":   "erin@alpha.example.com",
			"externalId": "okta-erin-1",
		}, "")
		if serr != nil {
			t.Fatalf("provision erin: %v", serr)
		}
		// "name" has no column in v1 and is directory-owned per ADR but unsupported.
		_, serr = ds.Update(ctx, scA, res.user.ID, &userPatch{
			Attributes: []attrChange{{Name: "name", Value: "Erin Q"}},
		}, "")
		if serr == nil || serr.Status != 400 {
			t.Fatalf("expected 400 for unsupported attribute, got %+v", serr)
		}
	})

	t.Run("scope isolation: workspace A cannot resolve/seed workspace B identities", func(t *testing.T) {
		resB, serr := ds.Provision(ctx, scB, map[string]any{
			"userName":   "dave@beta.example.com",
			"externalId": "okta-dave-1",
		}, "")
		if serr != nil {
			t.Fatalf("provision dave (B): %v", serr)
		}
		_, serr = ds.Get(ctx, scA, resB.user.ID)
		if serr == nil || serr.Status != 404 {
			t.Fatalf("expected 404 for cross-scope GET, got %+v", serr)
		}
		rows, serr := ds.Filter(ctx, scA, `externalId eq "okta-dave-1"`)
		if serr != nil {
			t.Fatalf("filter A: %v", serr)
		}
		if len(rows) != 0 {
			t.Fatalf("scope leak: A saw B's user (%d rows)", len(rows))
		}
	})

	t.Run("update of a non-SCIM-owned user is refused", func(t *testing.T) {
		jitUser := uuid.NewString()
		if _, err := pool.Exec(ctx,
			`INSERT INTO users (id, tenant_id, email, provider, provider_sub, role, status, provisioned_by, provisioning_owner)
			 VALUES ($1,$2,$3,$4,$5,'member','active','manual','manual')`,
			jitUser, wsA, "eve@alpha.example.com", "okta", "okta-eve-1",
		); err != nil {
			t.Fatalf("seed manual user: %v", err)
		}
		active := false
		_, serr := ds.Update(ctx, scA, jitUser, &userPatch{Active: &active}, "")
		if serr == nil || serr.Status != 409 {
			t.Fatalf("expected 409 for non-SCIM owned update, got %+v", serr)
		}
	})

	t.Run("SCIM disabled connection is rejected at the scope gate (fail-closed)", func(t *testing.T) {
		connOff := seedSCIMConnection(ctx, t, pool, wsA, "okta", "sub", "externalId")
		if _, err := pool.Exec(ctx,
			`UPDATE identity_connections SET scim_enabled = FALSE WHERE id = $1`, connOff); err != nil {
			t.Fatalf("disable connection: %v", err)
		}
		_, serr := ds.resolveScope(ctx, wsA, connOff)
		if serr == nil || serr.Status != 403 {
			t.Fatalf("expected 403 for disabled connection scope, got %+v", serr)
		}
	})
}

// ── seed + harness ─────────────────────────────────────────────────────────────

func seedWorkspace(ctx context.Context, t *testing.T, pool *pgxpool.Pool, slug string) string {
	t.Helper()
	id := uuid.NewString()
	if _, err := pool.Exec(ctx,
		`INSERT INTO workspaces (id, slug, name, status, trust_domain)
		 VALUES ($1,$2,$3,'active',$4)`,
		id, slug, slug+" Corp", "td-"+slug,
	); err != nil {
		t.Fatalf("seed workspace %s: %v", slug, err)
	}
	return id
}

func seedSCIMConnection(ctx context.Context, t *testing.T, pool *pgxpool.Pool, ws, provider, subjectClaim, scimIdent string) string {
	t.Helper()
	id := uuid.NewString()
	issuer := "https://" + provider + "-" + id[:8] + ".example.com"
	if _, err := pool.Exec(ctx,
		`INSERT INTO identity_connections
		   (id, tenant_id, protocol, provider, managed, display_name, issuer,
		    client_id, status, subject_claim, scim_identifier, scim_enabled)
		 VALUES ($1,$2,'oidc',$3,FALSE,$4,$5,gen_random_uuid(),'active',$6,$7,TRUE)`,
		id, ws, provider, provider+" Conn", issuer, subjectClaim, scimIdent,
	); err != nil {
		t.Fatalf("seed connection: %v", err)
	}
	return id
}

func scopeFor(t *testing.T, ctx context.Context, ds *DirectoryService, ws, conn string) *scope {
	t.Helper()
	sc, serr := ds.resolveScope(ctx, ws, conn)
	if serr != nil {
		t.Fatalf("resolve scope: %+v", serr)
	}
	return sc
}

func applyAllMigrations(ctx context.Context, pool *pgxpool.Pool) error {
	dir, err := filepath.Abs(filepath.Join("..", "..", "migrations"))
	if err != nil {
		return err
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil {
		return err
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		if _, err := pool.Exec(ctx, string(b)); err != nil {
			return fmt.Errorf("execute %s: %w", f, err)
		}
	}
	return nil
}
