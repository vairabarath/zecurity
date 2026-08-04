-- 031_identity_federation.sql
--
-- PENDING-04 / ADR-023 (Identity Philosophy) + ADR-024 (Identity Linking).
-- Stands up the two-tier identity federation model:
--
--   * Platform IdPs   (tenant_id IS NULL): built-in providers whose OAuth client
--     lives in platform env config (Google today; GitHub/LinkedIn later). Managed
--     by the provider/super-admin plane; available to every workspace by default.
--   * Workspace IdPs  (tenant_id set): a workspace's own BYO OIDC connection with
--     its client secret encrypted at rest (pki.EncryptSecret). Managed by the
--     workspace admin.
--
-- Greenfield: the app is not yet deployed, so there is NO user backfill. Existing
-- login continues to use users.provider/provider_sub until Phase 5 rewires the
-- code onto external_identities (after which those columns are dropped).

-- ── Connections (both tiers in one table; tier = whether tenant_id is NULL) ──
CREATE TABLE identity_connections (
    id                      UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    -- NULL  = platform-global IdP (creds resolved from env by `provider`).
    -- Set   = workspace-owned BYO IdP.
    tenant_id               UUID        REFERENCES workspaces(id) ON DELETE CASCADE,
    protocol                TEXT        NOT NULL DEFAULT 'oidc'
                                        CHECK (protocol IN ('oidc', 'saml')), -- 'saml' reserved, not built
    provider                TEXT        NOT NULL,   -- resolver key: 'google','github','okta','entra','oidc'
    managed                 BOOLEAN     NOT NULL DEFAULT FALSE, -- TRUE = platform; creds from env, no stored secret
    display_name            TEXT        NOT NULL,
    issuer                  TEXT        NOT NULL,
    client_id               TEXT,       -- NULL for managed (env-sourced)
    encrypted_client_secret TEXT,       -- NULL for managed; else base64 pki.EncryptSecret output
    secret_nonce            TEXT,       -- NULL for managed; else base64 nonce
    discovery_url           TEXT,       -- optional; else derived from issuer (.well-known)
    scopes                  TEXT        NOT NULL DEFAULT 'openid email profile',
    domain_hint             TEXT,       -- optional email-domain -> candidate IdP (selection hint only)
    claim_mappings          JSONB,      -- optional email/groups/name claim overrides
    status                  TEXT        NOT NULL DEFAULT 'active'
                                        CHECK (status IN ('active', 'disabled')),
    created_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- At most one platform connection per provider (global).
CREATE UNIQUE INDEX idx_idp_conn_platform  ON identity_connections (provider)          WHERE tenant_id IS NULL;
-- At most one workspace connection per issuer within a tenant.
CREATE UNIQUE INDEX idx_idp_conn_ws_issuer ON identity_connections (tenant_id, issuer) WHERE tenant_id IS NOT NULL;
-- Login resolution reads active connections for a workspace.
CREATE INDEX        idx_idp_conn_tenant    ON identity_connections (tenant_id)         WHERE status = 'active';

-- ── External identities: many external logins -> one canonical Zecurity user ──
-- tenant_id is denormalized from users so the uniqueness key stays per-tenant even
-- when connection_id points at a SHARED platform connection (ADR-024): the same
-- Google account in two workspaces is still two distinct users.
CREATE TABLE external_identities (
    id            UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     UUID        NOT NULL REFERENCES workspaces(id)           ON DELETE CASCADE,
    user_id       UUID        NOT NULL REFERENCES users(id)                ON DELETE CASCADE,
    connection_id UUID        NOT NULL REFERENCES identity_connections(id) ON DELETE CASCADE,
    issuer        TEXT        NOT NULL,
    subject       TEXT        NOT NULL,   -- immutable per-issuer subject (OIDC sub)
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- Issuer-scoped identity key: fixes the two-IdP `sub` collision that
    -- users.UNIQUE(tenant_id, provider_sub) allowed. Never key identity on email.
    UNIQUE (tenant_id, connection_id, subject)
);
CREATE INDEX idx_external_identities_user   ON external_identities (user_id);
CREATE INDEX idx_external_identities_lookup ON external_identities (tenant_id, connection_id, subject);

-- ── Canonical-user lifecycle + session generation (ADR-023) ──
-- 'invited' deliberately NOT added here: invite lifecycle lives in
-- workspace_members.status (the membership source of truth). 'locked' is the new
-- admin-lock state on the canonical user, independent of any IdP.
ALTER TABLE users DROP CONSTRAINT users_status_check;
ALTER TABLE users ADD  CONSTRAINT users_status_check
    CHECK (status IN ('active', 'suspended', 'deleted', 'locked'));

-- identity_generation backs mass session revocation (Phase 5): bumping it
-- invalidates every previously issued JWT/refresh for the user (e.g. on suspend,
-- connection removal, provider migration).
ALTER TABLE users ADD COLUMN identity_generation INT NOT NULL DEFAULT 1;

-- ── Seed the built-in Google platform IdP ──
-- Managed: client_id/secret are resolved at login from GOOGLE_CLIENT_ID/SECRET env
-- by provider name, so no credential is stored here.
INSERT INTO identity_connections (tenant_id, protocol, provider, managed, display_name, issuer)
VALUES (NULL, 'oidc', 'google', TRUE, 'Google', 'https://accounts.google.com');
