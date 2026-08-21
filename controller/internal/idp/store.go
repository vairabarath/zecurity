// Package idp is the connection-config store for the identity federation layer
// (PENDING-04 / ADR-024). It owns identity_connections rows only — both platform
// IdPs (tenant_id NULL, managed, creds from env) and per-workspace BYO IdPs
// (tenant_id set, client secret encrypted at rest via pki.Service).
//
// It does NOT own external_identities or the linking algorithm — those are
// identity concerns handled by internal/identity (Phase 5). This package answers
// "which IdPs can this workspace use, and how is each configured?" and nothing
// about users or sessions.
package idp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourorg/ztna/controller/internal/pki"
)

// ErrConnectionNotFound is returned when no connection matches the lookup.
var ErrConnectionNotFound = errors.New("identity connection not found")

// ErrWorkspaceNotFound is returned by WorkspaceIDBySlug when no workspace matches.
var ErrWorkspaceNotFound = errors.New("workspace not found")

// secretContext is the HKDF domain-separation label for a workspace's IdP client
// secret. Distinct from the bare tenantID that workspace CA keys derive under, so
// the two keys can never collide (pki.EncryptSecret).
func secretContext(tenantID string) string { return "idp-client-secret:" + tenantID }

// Connection is one identity_connections row. Platform connections have
// TenantID == nil and Managed == true (client credentials are resolved from env
// by Provider at login — never stored). Workspace connections have TenantID set
// and carry a client secret encrypted at rest; ClientSecret below is the
// decrypted value and MUST never be serialized back out to an API.
type Connection struct {
	ID            string
	TenantID      *string // nil = platform-global
	Protocol      string  // "oidc" | "saml"
	Provider      string  // "google","github","okta","entra","oidc",...
	Managed       bool
	DisplayName   string
	Issuer        string
	ClientID      string
	ClientSecret  string // decrypted; empty for managed; NEVER expose outward
	DiscoveryURL  string
	Scopes        string
	DomainHint    string
	ClaimMappings map[string]any
	Status        string // "active" | "disabled" | "deleted"
	// Mapping configuration (ADR-025 §3.1). These are the per-connection
	// overrides of the identity-mapping extractors. subjectClaim is the OIDC
	// claim the login adapter reads to produce AuthenticationContext.Subject;
	// scimIdentifier is the SCIM attribute the provisioning path reads to
	// produce the same Canonical Identity Key. Both MUST resolve to the same
	// value for the same person, but they are NOT assumed equal (never
	// hardcode sub == externalId).
	SubjectClaim   string
	ScimIdentifier string
	// ScimEnabled reports whether SCIM provisioning is permitted for this
	// connection. False by default and always false unless the mapping was
	// proven (Phase 5 round-trip) or explicitly overridden via the
	// identity.mapping.break_glass permission (Phase 4 §3.2).
	ScimEnabled bool
}

// CreateInput is the mutable field set for a workspace (BYO) connection.
// ClientSecret is plaintext here; it is encrypted before storage.
type CreateInput struct {
	Protocol      string // default "oidc"
	Provider      string
	DisplayName   string
	Issuer        string
	ClientID      string
	ClientSecret  string
	DiscoveryURL  string
	Scopes        string // default "openid email profile"
	DomainHint    string
	ClaimMappings map[string]any
}

// Store reads/writes identity_connections. enc encrypts/decrypts workspace client
// secrets at rest.
type Store struct {
	pool *pgxpool.Pool
	enc  pki.Service
}

func NewStore(pool *pgxpool.Pool, enc pki.Service) *Store {
	return &Store{pool: pool, enc: enc}
}

const connColumns = `id, tenant_id, protocol, provider, managed, display_name, issuer,
	client_id, encrypted_client_secret, secret_nonce, discovery_url, scopes,
	domain_hint, claim_mappings, status, subject_claim, scim_identifier, scim_enabled`

type scannable interface {
	Scan(dest ...any) error
}

// scanConnection maps a row into a Connection, decrypting the client secret for
// workspace (non-managed) connections.
func (s *Store) scanConnection(row scannable) (*Connection, error) {
	var (
		c                                                       Connection
		tenantID, clientID, encSecret, nonce, discovery, domain *string
		claimMappings                                           []byte
	)
	if err := row.Scan(
		&c.ID, &tenantID, &c.Protocol, &c.Provider, &c.Managed, &c.DisplayName, &c.Issuer,
		&clientID, &encSecret, &nonce, &discovery, &c.Scopes,
		&domain, &claimMappings, &c.Status, &c.SubjectClaim, &c.ScimIdentifier, &c.ScimEnabled,
	); err != nil {
		return nil, err
	}
	c.TenantID = tenantID
	if clientID != nil {
		c.ClientID = *clientID
	}
	if discovery != nil {
		c.DiscoveryURL = *discovery
	}
	if domain != nil {
		c.DomainHint = *domain
	}
	if len(claimMappings) > 0 {
		if err := json.Unmarshal(claimMappings, &c.ClaimMappings); err != nil {
			return nil, fmt.Errorf("decode claim_mappings: %w", err)
		}
	}
	if !c.Managed && encSecret != nil && nonce != nil && tenantID != nil {
		pt, err := s.enc.DecryptSecret(*encSecret, *nonce, secretContext(*tenantID))
		if err != nil {
			return nil, fmt.Errorf("decrypt client secret: %w", err)
		}
		c.ClientSecret = string(pt)
	}
	return &c, nil
}

// ListForWorkspace returns the IdPs a workspace can use: the platform IdPs
// (tenant_id IS NULL) plus the workspace's own connections. Platform IdPs are
// listed first. This is the single resolution query — an unconfigured workspace
// simply gets the platform IdPs (e.g. Google) back.
func (s *Store) ListForWorkspace(ctx context.Context, tenantID string) ([]Connection, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+connColumns+` FROM identity_connections
		 WHERE tenant_id IS NULL OR tenant_id = $1
		 ORDER BY (tenant_id IS NOT NULL), display_name`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("query connections: %w", err)
	}
	defer rows.Close()

	var out []Connection
	for rows.Next() {
		c, err := s.scanConnection(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

// GetByID returns a connection by id, or ErrConnectionNotFound.
func (s *Store) GetByID(ctx context.Context, id string) (*Connection, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+connColumns+` FROM identity_connections WHERE id = $1`, id)
	c, err := s.scanConnection(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrConnectionNotFound
	}
	return c, err
}

// GetPlatformByProvider returns the active platform (Bootstrap-tier) connection
// for a provider — the tenant_id-NULL row, e.g. the built-in Google. Used to
// resolve the bootstrap login before any workspace is known.
func (s *Store) GetPlatformByProvider(ctx context.Context, provider string) (*Connection, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT `+connColumns+` FROM identity_connections
		 WHERE provider = $1 AND tenant_id IS NULL AND status = 'active'`, provider)
	c, err := s.scanConnection(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrConnectionNotFound
	}
	return c, err
}

// GetByIssuer resolves a connection usable by a workspace for a given issuer,
// preferring the workspace's own connection over a platform one.
func (s *Store) GetByIssuer(ctx context.Context, tenantID, issuer string) (*Connection, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT `+connColumns+` FROM identity_connections
		 WHERE issuer = $2 AND (tenant_id = $1 OR tenant_id IS NULL)
		 ORDER BY (tenant_id IS NULL)
		 LIMIT 1`, tenantID, issuer)
	c, err := s.scanConnection(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrConnectionNotFound
	}
	return c, err
}

// CreateWorkspaceConnection inserts a workspace-owned (BYO) OIDC connection,
// encrypting the client secret at rest. Platform connections are never created
// here (they are seeded / provider-managed).
func (s *Store) CreateWorkspaceConnection(ctx context.Context, tenantID string, in CreateInput) (*Connection, error) {
	protocol := in.Protocol
	if protocol == "" {
		protocol = "oidc"
	}
	scopes := in.Scopes
	if scopes == "" {
		scopes = "openid email profile"
	}

	var encSecret, nonce *string
	if in.ClientSecret != "" {
		ct, n, err := s.enc.EncryptSecret([]byte(in.ClientSecret), secretContext(tenantID))
		if err != nil {
			return nil, fmt.Errorf("encrypt client secret: %w", err)
		}
		encSecret, nonce = &ct, &n
	}

	var claimJSON []byte
	if in.ClaimMappings != nil {
		b, err := json.Marshal(in.ClaimMappings)
		if err != nil {
			return nil, fmt.Errorf("encode claim_mappings: %w", err)
		}
		claimJSON = b
	}

	row := s.pool.QueryRow(ctx,
		`INSERT INTO identity_connections
		 (tenant_id, protocol, provider, managed, display_name, issuer, client_id,
		  encrypted_client_secret, secret_nonce, discovery_url, scopes, domain_hint, claim_mappings)
		 VALUES ($1,$2,$3,FALSE,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		 RETURNING `+connColumns,
		tenantID, protocol, in.Provider, in.DisplayName, in.Issuer, nullIfEmpty(in.ClientID),
		encSecret, nonce, nullIfEmpty(in.DiscoveryURL), scopes, nullIfEmpty(in.DomainHint), claimJSON)
	return s.scanConnection(row)
}

// SetStatus enables/disables a workspace connection. The tenant_id guard means a
// workspace can only toggle its own connections, never a platform one.
func (s *Store) SetStatus(ctx context.Context, tenantID, id, status string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE identity_connections SET status = $3, updated_at = NOW()
		 WHERE id = $1 AND tenant_id = $2`, id, tenantID, status)
	if err != nil {
		return fmt.Errorf("update status: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrConnectionNotFound
	}
	return nil
}

// SetSCIMEnabled sets the SCIM-enablement flag for a workspace connection.
//
// The tenant_id guard scopes it to the caller's workspace. This flag is the
// fail-closed result of mapping validation (Phase 4): it is FALSE unless the
// mapping was proven (Phase 5 round-trip) or explicitly overridden via the
// identity.mapping.break_glass permission (Phase 4 §3.2). Normal admins
// cannot flip it — only the mapping gate / break-glass path sets it.
func (s *Store) SetSCIMEnabled(ctx context.Context, tenantID, id string, enabled bool) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE identity_connections SET scim_enabled = $3, updated_at = NOW()
		 WHERE id = $1 AND tenant_id = $2`, id, tenantID, enabled)
	if err != nil {
		return fmt.Errorf("update scim_enabled: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrConnectionNotFound
	}
	return nil
}

// DeleteWorkspaceConnection removes a workspace connection. The tenant_id guard
// prevents deleting a platform (tenant_id NULL) connection through this path.
func (s *Store) DeleteWorkspaceConnection(ctx context.Context, tenantID, id string) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM identity_connections WHERE id = $1 AND tenant_id = $2`, id, tenantID)
	if err != nil {
		return fmt.Errorf("delete connection: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrConnectionNotFound
	}
	return nil
}

// UpdateInput is the mutable field set for updating a workspace connection.
// A nil pointer leaves that column unchanged; a non-nil ClientSecret replaces
// the encrypted secret (an empty string is a valid — if unusual — new secret).
// Issuer and provider are immutable after creation (they anchor the identity
// key), so they are not updatable here.
type UpdateInput struct {
	DisplayName  *string
	ClientID     *string
	ClientSecret *string
	DiscoveryURL *string
	Scopes       *string
	DomainHint   *string
}

// UpdateWorkspaceConnection applies a partial update to a workspace (BYO)
// connection. The `managed = FALSE` guard means a platform connection can never
// be edited through this path, and the tenant_id guard scopes it to the caller's
// workspace. The client secret is re-encrypted only when a new one is supplied.
func (s *Store) UpdateWorkspaceConnection(ctx context.Context, tenantID, id string, in UpdateInput) (*Connection, error) {
	var encSecret, nonce *string
	setSecret := false
	if in.ClientSecret != nil {
		ct, n, err := s.enc.EncryptSecret([]byte(*in.ClientSecret), secretContext(tenantID))
		if err != nil {
			return nil, fmt.Errorf("encrypt client secret: %w", err)
		}
		encSecret, nonce, setSecret = &ct, &n, true
	}

	row := s.pool.QueryRow(ctx,
		`UPDATE identity_connections SET
		    display_name = COALESCE($3, display_name),
		    client_id    = COALESCE($4, client_id),
		    discovery_url= COALESCE($5, discovery_url),
		    scopes       = COALESCE($6, scopes),
		    domain_hint  = COALESCE($7, domain_hint),
		    encrypted_client_secret = CASE WHEN $8 THEN $9  ELSE encrypted_client_secret END,
		    secret_nonce            = CASE WHEN $8 THEN $10 ELSE secret_nonce END,
		    updated_at = NOW()
		  WHERE id = $1 AND tenant_id = $2 AND managed = FALSE
		  RETURNING `+connColumns,
		id, tenantID, in.DisplayName, in.ClientID, in.DiscoveryURL, in.Scopes, in.DomainHint,
		setSecret, encSecret, nonce)
	c, err := s.scanConnection(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrConnectionNotFound
	}
	return c, err
}

// ListWorkspaceConnections returns ONLY the workspace's own (BYO) connections —
// not the platform IdPs. This is the admin management view (create/edit/delete),
// as distinct from ListForWorkspace, which is the login-resolution view that
// also includes the shared platform IdPs.
func (s *Store) ListWorkspaceConnections(ctx context.Context, tenantID string) ([]Connection, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+connColumns+` FROM identity_connections
		 WHERE tenant_id = $1
		 ORDER BY display_name`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("query workspace connections: %w", err)
	}
	defer rows.Close()

	var out []Connection
	for rows.Next() {
		c, err := s.scanConnection(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

// UserIDsForConnection returns the canonical users that have an external
// identity through this connection — the set whose sessions must be revoked
// when the connection is disabled or deleted (their login path is gone).
func (s *Store) UserIDsForConnection(ctx context.Context, tenantID, connectionID string) ([]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT DISTINCT user_id::text FROM external_identities
		 WHERE tenant_id = $1 AND connection_id = $2`, tenantID, connectionID)
	if err != nil {
		return nil, fmt.Errorf("query users for connection: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// CountActiveWorkspaceConnections counts a workspace's own active connections.
// Used by the no-lockout guard before disabling/deleting a connection.
func (s *Store) CountActiveWorkspaceConnections(ctx context.Context, tenantID string) (int, error) {
	var n int
	if err := s.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM identity_connections
		 WHERE tenant_id = $1 AND status = 'active'`, tenantID).Scan(&n); err != nil {
		return 0, fmt.Errorf("count active workspace connections: %w", err)
	}
	return n, nil
}

// PlatformLoginEnabled reports whether the workspace still offers the shared
// platform IdP as a login path (workspaces.platform_login_enabled, default true).
// Read by the discovery endpoint and by the no-lockout guard.
func (s *Store) PlatformLoginEnabled(ctx context.Context, tenantID string) (bool, error) {
	var enabled bool
	err := s.pool.QueryRow(ctx,
		`SELECT platform_login_enabled FROM workspaces WHERE id = $1`, tenantID).Scan(&enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrWorkspaceNotFound
	}
	if err != nil {
		return false, fmt.Errorf("read platform_login_enabled: %w", err)
	}
	return enabled, nil
}

// SetPlatformLoginEnabled flips the workspace's platform login toggle. Callers
// (the admin mutation) are responsible for the no-lockout guard before disabling.
func (s *Store) SetPlatformLoginEnabled(ctx context.Context, tenantID string, enabled bool) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE workspaces SET platform_login_enabled = $2, updated_at = NOW() WHERE id = $1`,
		tenantID, enabled)
	if err != nil {
		return fmt.Errorf("set platform_login_enabled: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrWorkspaceNotFound
	}
	return nil
}

// WorkspaceIDBySlug resolves a workspace slug to its id. Used by the login
// discovery endpoint to map a workspace-first request to its connections.
func (s *Store) WorkspaceIDBySlug(ctx context.Context, slug string) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, `SELECT id::text FROM workspaces WHERE slug = $1`, slug).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrWorkspaceNotFound
	}
	if err != nil {
		return "", fmt.Errorf("lookup workspace by slug: %w", err)
	}
	return id, nil
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
