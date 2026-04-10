package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yourorg/ztna/controller/internal/appmeta" // WorkspaceTrustDomain for bootstrap INSERT
	"github.com/yourorg/ztna/controller/internal/pki"
)

// Result is returned by Bootstrap and consumed by auth code when issuing JWTs.
type Result struct {
	TenantID string
	UserID   string
	Role     string
}

// Service owns the dependencies required for bootstrap provisioning.
type Service struct {
	Pool       *pgxpool.Pool
	PKIService pki.Service
}

// Bootstrap creates a new workspace for a first-time user or returns the
// existing membership for a returning user.
func (s *Service) Bootstrap(
	ctx context.Context,
	email, provider, providerSub, name string,
) (*Result, error) {
	var existingUserID string
	var existingTenantID string
	var existingRole string

	err := s.Pool.QueryRow(
		ctx,
		`SELECT id, tenant_id, role
		 FROM users
		 WHERE provider_sub = $1 AND provider = $2
		 LIMIT 1`,
		providerSub,
		provider,
	).Scan(&existingUserID, &existingTenantID, &existingRole)

	if err == nil {
		_, updateErr := s.Pool.Exec(
			ctx,
			`UPDATE users
			 SET last_login_at = NOW(), updated_at = NOW()
			 WHERE id = $1`,
			existingUserID,
		)
		if updateErr != nil {
			fmt.Printf("warning: update last_login_at failed for user %s: %v\n", existingUserID, updateErr)
		}

		return &Result{
			TenantID: existingTenantID,
			UserID:   existingUserID,
			Role:     existingRole,
		}, nil
	}

	if !isNoRows(err) {
		return nil, fmt.Errorf("lookup user by provider_sub: %w", err)
	}

	return s.runBootstrapTransaction(ctx, email, provider, providerSub, name)
}

func (s *Service) runBootstrapTransaction(
	ctx context.Context,
	email, provider, providerSub, name string,
) (*Result, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	slug := slugify(name)
	trustDomain := "ws-" + slug + ".zecurity.in"

	// SPIFFE trust domain derived from workspace slug.
	// Required since migration 002 makes trust_domain NOT NULL.
	trustDomain := appmeta.WorkspaceTrustDomain(slug)

	var tenantID string
	err = tx.QueryRow(
		ctx,
		`INSERT INTO workspaces (slug, name, status, trust_domain)
		 VALUES ($1, $2, 'provisioning', $3)
		 RETURNING id`,
		slug,
		name,
		trustDomain,
	).Scan(&tenantID)
	if err != nil {
		return nil, fmt.Errorf("insert workspace: %w", err)
	}

	var userID string
	err = tx.QueryRow(
		ctx,
		`INSERT INTO users
		 (tenant_id, email, provider, provider_sub, role, status)
		 VALUES ($1, $2, $3, $4, 'admin', 'active')
		 RETURNING id`,
		tenantID,
		email,
		provider,
		providerSub,
	).Scan(&userID)
	if err != nil {
		return nil, fmt.Errorf("insert user: %w", err)
	}

	caResult, err := s.PKIService.GenerateWorkspaceCA(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("generate workspace CA: %w", err)
	}

	_, err = tx.Exec(
		ctx,
		`INSERT INTO workspace_ca_keys
		 (tenant_id, encrypted_private_key, nonce, key_algorithm,
		  certificate_pem, not_before, not_after)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		tenantID,
		caResult.EncryptedPrivateKey,
		caResult.Nonce,
		caResult.KeyAlgorithm,
		caResult.CertificatePEM,
		caResult.NotBefore,
		caResult.NotAfter,
	)
	if err != nil {
		return nil, fmt.Errorf("insert workspace_ca_keys: %w", err)
	}

	_, err = tx.Exec(
		ctx,
		`UPDATE workspaces
		 SET status = 'active',
		     ca_cert_pem = $1,
		     updated_at = NOW()
		 WHERE id = $2`,
		caResult.CertificatePEM,
		tenantID,
	)
	if err != nil {
		return nil, fmt.Errorf("activate workspace: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit bootstrap transaction: %w", err)
	}

	return &Result{
		TenantID: tenantID,
		UserID:   userID,
		Role:     "admin",
	}, nil
}

// slugify converts a display name into a URL-safe lowercase slug.
func slugify(name string) string {
	var b strings.Builder
	prev := '-'

	for _, r := range strings.ToLower(name) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			prev = r
			continue
		}

		if prev != '-' {
			b.WriteRune('-')
			prev = '-'
		}
	}

	slug := strings.Trim(b.String(), "-")
	if slug == "" {
		return "workspace"
	}

	return slug
}

func isNoRows(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}
