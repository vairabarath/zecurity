package scim

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ── pure gate behavior (no DB) ──────────────────────────────────────────────

func TestGate_UnprovenIsFailClosed(t *testing.T) {
	gate := NewMappingGate("okta")
	res, err := gate.Evaluate(context.Background(), "sub", "externalId", BreakGlassOverride{})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if res.MappingState != MappingUnproven {
		t.Fatalf("MappingState = %q, want %q (Phase 4 never reports proven without a round-trip)", res.MappingState, MappingUnproven)
	}
	if res.ScimEnabledAllowed {
		t.Fatalf("unproven mapping must NOT allow SCIM (fail-closed)")
	}
	if res.RoundTripVerified {
		t.Fatalf("RoundTripVerified must be false in Phase 4")
	}
	if res.Reason == "" {
		t.Fatalf("Reason must always be populated")
	}
}

func TestGate_InvalidConfigNeverProven(t *testing.T) {
	// A config that equates both extractors is rejected by the gate (never
	// silently "proven" and never silently enabled).
	gate := NewMappingGate("entra")
	res, err := gate.Evaluate(context.Background(), "sub", "sub", BreakGlassOverride{})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if res.MappingState != MappingUnproven {
		t.Fatalf("invalid config must remain unproven, got %q", res.MappingState)
	}
	if res.ScimEnabledAllowed {
		t.Fatalf("invalid config must keep SCIM disabled")
	}
}

func TestGate_BreakGlassOverrideAllowsDespiteUnproven(t *testing.T) {
	gate := NewMappingGate("okta")
	override := BreakGlassOverride{
		Applied:    true,
		Actor:      "admin-1",
		Reason:     "read-only IdP, verified out of band",
		Workspace:  "ws-1",
		Connection: "conn-1",
	}
	res, err := gate.Evaluate(context.Background(), "sub", "externalId", override)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	// Override permits enabling SCIM, but the mapping remains NOT "proven".
	if !res.ScimEnabledAllowed {
		t.Fatalf("break_glass override must allow SCIM")
	}
	if res.MappingState == MappingProven {
		t.Fatalf("an override must NOT retroactively mark the mapping as proven")
	}
}

func TestGate_BreakGlassWithoutReasonDenied(t *testing.T) {
	// Applied=true but empty reason is not a valid override (resolver enforces
	// this too, but the gate must not enable on a blank reason).
	gate := NewMappingGate("okta")
	res, err := gate.Evaluate(context.Background(), "sub", "externalId", BreakGlassOverride{Applied: true, Reason: ""})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if res.ScimEnabledAllowed {
		t.Fatalf("override without a reason must not enable SCIM")
	}
}

func TestResult_WithRoundTripSeam(t *testing.T) {
	// The Phase 5 seam: only a VERIFIED round-trip flips the state to proven.
	base := MappingGateResult{MappingState: MappingUnproven, ScimEnabledAllowed: false, RoundTripVerified: false}
	proven := base.WithRoundTrip(true, "sub", "externalId")
	if proven.MappingState != MappingProven {
		t.Fatalf("verified round-trip must set MappingState=proven, got %q", proven.MappingState)
	}
	if !proven.ScimEnabledAllowed {
		t.Fatalf("proven mapping must allow SCIM")
	}
	if !proven.RoundTripVerified {
		t.Fatalf("proven result must carry RoundTripVerified=true")
	}

	// A FALSE verification must not change the (fail-closed) base result.
	stillUnproven := base.WithRoundTrip(false, "sub", "externalId")
	if stillUnproven.MappingState != MappingUnproven || stillUnproven.ScimEnabledAllowed {
		t.Fatalf("false round-trip must keep state unproven and SCIM disabled")
	}
}

// ── integration (DB) ─────────────────────────────────────────────────────────

func TestGate_Integration(t *testing.T) {
	adminDSN := os.Getenv("PKI_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("PKI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()

	// Use an isolated child database so this test does not depend on, nor
	// collide with, the shared admin DSN's schema state (mirrors the
	// pipeline/outbox integration harnesses).
	adminPool, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	defer adminPool.Close()
	dbName := "scim_gate_test_" + fmt.Sprint(os.Getpid())
	_, _ = adminPool.Exec(ctx, "DROP DATABASE IF EXISTS "+dbName)
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	defer func() { _, _ = adminPool.Exec(ctx, "DROP DATABASE IF EXISTS "+dbName) }()
	testDSN, err := withDBName(adminDSN, dbName)
	if err != nil {
		t.Fatalf("build test dsn: %v", err)
	}
	pool, err := pgxpool.New(ctx, testDSN)
	if err != nil {
		t.Fatalf("connect test db: %v", err)
	}
	defer pool.Close()
	if err := applyAllMigrations(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	var ws, conn, admin string
	if err := pool.QueryRow(ctx,
		`INSERT INTO workspaces (slug, name, status, trust_domain)
		 VALUES ('gate-' || gen_random_uuid()::text, 'gate', 'active', 'gate.test') RETURNING id::text`,
	).Scan(&ws); err != nil {
		t.Fatalf("workspace: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO identity_connections (tenant_id, protocol, provider, managed, display_name, issuer, status, subject_claim, scim_identifier, scim_enabled)
		 VALUES ($1,'oidc','okta',FALSE,'okta','https://okta.example','active','sub','externalId',FALSE) RETURNING id::text`,
		ws,
	).Scan(&conn); err != nil {
		t.Fatalf("connection: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (tenant_id, email, provider, provider_sub, role)
		 VALUES ($1,'gadmin@test','test','gadmin','admin') RETURNING id::text`,
		ws,
	).Scan(&admin); err != nil {
		t.Fatalf("user: %v", err)
	}

	t.Run("unproven → SCIM disabled, fail-closed", func(t *testing.T) {
		gate := NewMappingGate("okta")
		res, err := gate.Evaluate(ctx, "sub", "externalId", BreakGlassOverride{})
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if res.ScimEnabledAllowed {
			t.Fatalf("without proof or override, SCIM must stay disabled")
		}
	})

	t.Run("break_glass grant + reason → SCIM enabled, audited path", func(t *testing.T) {
		// Grant the dedicated permission to the admin.
		if _, err := pool.Exec(ctx,
			`INSERT INTO workspace_permissions (workspace_id, user_id, permission, granted_by)
			 VALUES ($1,$2,'identity.mapping.break_glass',$2)`, ws, admin); err != nil {
			t.Fatalf("grant break_glass: %v", err)
		}
		// Confirm the permission store answers true (mirrors the resolver check).
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM workspace_permissions WHERE workspace_id=$1 AND user_id=$2 AND permission='identity.mapping.break_glass'`,
			ws, admin).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		if n != 1 {
			t.Fatalf("break_glass grant row missing (got %d)", n)
		}
		// The gate, given an applied override, allows SCIM.
		gate := NewMappingGate("okta")
		res, err := gate.Evaluate(ctx, "sub", "externalId", BreakGlassOverride{
			Applied: true, Actor: admin, Reason: "entra read-only probe not supported", Workspace: ws, Connection: conn,
		})
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if !res.ScimEnabledAllowed {
			t.Fatalf("break_glass override must allow SCIM enabling")
		}
		if res.MappingState == MappingProven {
			t.Fatalf("override must not mark mapping proven")
		}
	})
}
