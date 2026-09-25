// Command phase8 is the Sprint 19 Phase 8 end-to-end harness (PENDING-16).
//
// Phase 8 must prove the real path
//
//	Linux device -> posture -> Device Profile -> Resource Policy -> ACL -> Connector -> ALLOW/DENY
//
// on real hardware. Two things that path needs are not reachable from outside
// the controller process, which is why this tool exists:
//
//   - bootstrap/token: the admin GraphQL API is guarded by a Bearer JWT that is
//     normally minted by the Google OAuth callback. Phase 8 proves posture and
//     ACL compilation, not Google's login flow, so the harness mints the same
//     HS256 token the callback would and seeds a workspace through the REAL
//     bootstrap service (real workspace CA, real admin user).
//
//   - acl: the compiled ACL snapshot has no read surface at all -- no GraphQL
//     query, no debug endpoint. Without it, "the passing device is in the
//     allowed identity set" is unobservable. This dumps exactly what
//     policy.CompileACLSnapshot produces, which is what the Connector enforces.
//
// This is test scaffolding for a local run. It reads DATABASE_URL and
// JWT_SECRET from the environment and never talks to the network.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourorg/ztna/controller/internal/appmeta"
	"github.com/yourorg/ztna/controller/internal/bootstrap"
	"github.com/yourorg/ztna/controller/internal/identity"
	"github.com/yourorg/ztna/controller/internal/pki"
	"github.com/yourorg/ztna/controller/internal/policy"
	"github.com/yourorg/ztna/controller/internal/posture"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, mustEnv("DATABASE_URL"))
	if err != nil {
		log.Fatalf("connect database: %v", err)
	}
	defer pool.Close()

	switch os.Args[1] {
	case "bootstrap":
		cmdBootstrap(ctx, pool)
	case "token":
		cmdToken(ctx, pool)
	case "acl":
		cmdACL(ctx, pool)
	case "enroll":
		cmdEnroll()
	default:
		usage()
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `phase8 -- Sprint 19 Phase 8 end-to-end harness

  bootstrap <email> <workspace-name>   create workspace + admin user + workspace CA
  token <email>                        mint an admin Bearer JWT for the GraphQL API
  acl <workspace-id>                   dump the compiled ACL snapshot as JSON
  enroll <token> <grpc> <http> <name>  enrol a device and print its daemon state

Environment: DATABASE_URL, JWT_SECRET, PKI_MASTER_SECRET
`)
	os.Exit(2)
}

// cmdBootstrap seeds a workspace through the same provisioner the OAuth
// callback uses, so the workspace CA and admin user are produced by production
// code rather than hand-written SQL.
func cmdBootstrap(ctx context.Context, pool *pgxpool.Pool) {
	if len(os.Args) < 4 {
		usage()
	}
	email, workspaceName := os.Args[2], os.Args[3]

	pkiSvc, err := pki.Init(ctx, pool)
	if err != nil {
		log.Fatalf("pki init: %v", err)
	}

	// external_identities.connection_id is a FK to the Bootstrap (platform) IdP
	// row -- the one with tenant_id IS NULL that migration 031 seeds. Look it up
	// rather than hardcoding, so this works against any freshly migrated database.
	var connectionID, issuer, provider string
	if err := pool.QueryRow(ctx,
		`SELECT id::text, issuer, provider
		   FROM identity_connections
		  WHERE tenant_id IS NULL
		  ORDER BY created_at
		  LIMIT 1`,
	).Scan(&connectionID, &issuer, &provider); err != nil {
		log.Fatalf("lookup bootstrap identity connection: %v", err)
	}

	svc := &bootstrap.Service{Pool: pool, PKIService: pkiSvc}
	principal, err := svc.Provision(ctx, identity.ProvisionInput{
		Email:         email,
		Provider:      provider,
		Subject:       "phase8-" + email,
		Name:          "Phase 8 Admin",
		ConnectionID:  connectionID,
		Issuer:        issuer,
		WorkspaceName: workspaceName,
	})
	if err != nil {
		log.Fatalf("provision: %v", err)
	}

	var slug, trustDomain string
	if err := pool.QueryRow(ctx,
		`SELECT slug, trust_domain FROM workspaces WHERE id = $1`,
		principal.TenantID,
	).Scan(&slug, &trustDomain); err != nil {
		log.Fatalf("read workspace: %v", err)
	}

	emit(map[string]string{
		"workspace_id": principal.TenantID,
		"user_id":      principal.UserID,
		"slug":         slug,
		"trust_domain": trustDomain,
		"role":         principal.Role,
	})
}

// cmdToken mints the access token the Google callback would have issued. The
// claim set is copied from auth.issueAccessToken; the middleware validates
// alg, issuer, expiry, sub, tenant_id and role.
func cmdToken(ctx context.Context, pool *pgxpool.Pool) {
	if len(os.Args) < 3 {
		usage()
	}
	email := os.Args[2]

	var userID, tenantID, role string
	if err := pool.QueryRow(ctx,
		`SELECT id::text, tenant_id::text, role FROM users WHERE email = $1`,
		email,
	).Scan(&userID, &tenantID, &role); err != nil {
		log.Fatalf("lookup user %q: %v", email, err)
	}

	now := time.Now()
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"tenant_id": tenantID,
		"role":      role,
		"email":     email,
		"sub":       userID,
		"iss":       appmeta.ControllerIssuer,
		"iat":       now.Unix(),
		"exp":       now.Add(12 * time.Hour).Unix(),
	})
	signed, err := token.SignedString([]byte(mustEnv("JWT_SECRET")))
	if err != nil {
		log.Fatalf("sign token: %v", err)
	}
	fmt.Println(signed)
}

// cmdACL compiles and prints the workspace ACL snapshot. This is the only way
// to observe allowed_spiffe_ids, which is the fact Phase 8 turns on: a device
// that satisfies its Device Profile appears here, one that fails does not.
func cmdACL(ctx context.Context, pool *pgxpool.Pool) {
	if len(os.Args) < 3 {
		usage()
	}
	workspaceID := os.Args[2]

	notifier := policy.NewNotifier(policy.NewSnapshotCache())
	compiled, err := policy.CompileACLSnapshot(
		ctx,
		policy.NewStore(pool),
		posture.NewStore(pool),
		notifier,
		workspaceID,
	)
	if err != nil {
		log.Fatalf("compile acl: %v", err)
	}
	emit(compiled)
}

func emit(v any) {
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		log.Fatalf("marshal: %v", err)
	}
	fmt.Println(string(out))
}

func mustEnv(key string) string {
	value := os.Getenv(key)
	if value == "" {
		log.Fatalf("%s is required", key)
	}
	return value
}
