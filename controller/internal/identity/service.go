package identity

import (
	"context"
	"fmt"
	"log"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourorg/ztna/controller/internal/auth/providers"
)

// Service orchestrates the identity pipeline for a browser/web login:
//
//	resolve → lifecycle-gate → (link / JIT-create) → Principal → event
//
// It is the single seam that auth (callback) and, later, graph import. Each
// stage is a small single-responsibility unit (Resolver, CheckLifecycle,
// Linker) reusable on its own by PENDING-05/06/08/09; this type just wires them
// in order and produces the AuthenticatedPrincipal pivot.
// identityResolver is the resolution seam (external_identities → canonical
// user). *Resolver satisfies it in production; tests inject a fake so the
// pipeline wiring is exercised without a database.
type identityResolver interface {
	Resolve(ctx context.Context, connectionID, subject, tenantID string) (*PrincipalCore, bool, error)
}

type Service struct {
	resolver  identityResolver
	linker    *Linker
	publisher EventPublisher
	pool      *pgxpool.Pool
}

// NewService constructs the pipeline. A nil publisher defaults to NopPublisher.
func NewService(pool *pgxpool.Pool, linker *Linker, publisher EventPublisher) *Service {
	if publisher == nil {
		publisher = NopPublisher{}
	}
	return &Service{
		resolver:  NewResolver(pool),
		linker:    linker,
		publisher: publisher,
		pool:      pool,
	}
}

// Authenticate turns a proven AuthenticationContext into a Principal.
//
// connectionID is the IdP connection that produced this login (the adapter
// switch point already validated the token against it). workspaceName is the
// display name for a brand-new workspace on first-time signup (else "").
//
// Web/platform login is not workspace-scoped, so resolution passes tenantID ""
// and takes the stable first match for a shared platform IdP — mirroring the
// pre-Phase-5 behavior. Fails closed: a resolved-but-inactive user is rejected.
func (s *Service) Authenticate(
	ctx context.Context,
	authCtx *providers.AuthenticationContext,
	connectionID, workspaceName string,
) (*Principal, error) {
	core, found, err := s.resolver.Resolve(ctx, connectionID, authCtx.Subject, "")
	if err != nil {
		return nil, fmt.Errorf("resolve identity: %w", err)
	}

	if found {
		if err := CheckLifecycle(core.Status); err != nil {
			return nil, err
		}
		s.touchLastLogin(core.UserID)
		return &Principal{Core: *core, Auth: authCtx}, nil
	}

	// First-seen identity → JIT-create (never email-merge — ADR-024).
	name := authCtx.Name
	if name == "" {
		name = authCtx.Email
	}
	created, err := s.linker.Link(ctx, ProvisionInput{
		Email:         authCtx.Email,
		Provider:      authCtx.Provider,
		Subject:       authCtx.Subject,
		Name:          name,
		ConnectionID:  connectionID,
		Issuer:        authCtx.Issuer,
		WorkspaceName: workspaceName,
	})
	if err != nil {
		return nil, fmt.Errorf("link identity: %w", err)
	}

	// Emit the provisioning event now that a tenant is known. A failed audit
	// write must not fail an otherwise-valid login (audit_logs is not on the
	// login availability path) — log and continue.
	if perr := s.publisher.Publish(ctx, Event{
		TenantID:    created.TenantID,
		ActorUserID: created.UserID,
		ActorEmail:  created.Email,
		Action:      ActionUserProvisioned,
		TargetType:  "user",
		TargetID:    created.UserID,
		Details:     map[string]any{"provider": authCtx.Provider, "connection_id": connectionID},
	}); perr != nil {
		log.Printf("identity: publish provisioned event for %s: %v", created.UserID, perr)
	}

	return &Principal{Core: *created, Auth: authCtx}, nil
}

// touchLastLogin updates last_login_at without adding latency to the login.
// Fire-and-forget on a background context: the request ctx may be canceled
// before the goroutine runs, and a slow metadata write must not block login.
func (s *Service) touchLastLogin(userID string) {
	if s.pool == nil {
		return
	}
	go func() {
		if _, err := s.pool.Exec(context.Background(),
			`UPDATE users SET last_login_at = NOW(), updated_at = NOW() WHERE id = $1`,
			userID,
		); err != nil {
			log.Printf("identity: update last_login_at for %s: %v", userID, err)
		}
	}()
}
