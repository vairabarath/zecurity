-- Sprint 17 / M1 / Phase 1
-- SCIM Directory Synchronization schema.
--
-- This migration is additive except for replacing legacy group-name
-- uniqueness and extending identity_connections.status.
--
-- PENDING-05 / ADR-025
-- Do NOT create outbox_events here; that belongs to 033_outbox_events.sql.

-- 1. Identity connections
--
-- status already exists in 031_identity_federation.sql.
-- Replace the existing active/disabled constraint so the connection can
-- reach the terminal deleted state used by SCIM lifecycle management.

ALTER TABLE identity_connections
    DROP CONSTRAINT IF EXISTS identity_connections_status_check;

ALTER TABLE identity_connections
    ADD CONSTRAINT identity_connections_status_check
    CHECK (status IN ('active', 'disabled', 'deleted'));

ALTER TABLE identity_connections
    ADD COLUMN IF NOT EXISTS subject_claim
        TEXT NOT NULL DEFAULT 'sub';

ALTER TABLE identity_connections
    ADD COLUMN IF NOT EXISTS scim_identifier
        TEXT NOT NULL DEFAULT 'externalId';

ALTER TABLE identity_connections
    ADD COLUMN IF NOT EXISTS scim_enabled
        BOOLEAN NOT NULL DEFAULT FALSE;

ALTER TABLE identity_connections
    ADD COLUMN IF NOT EXISTS last_sync_at
        TIMESTAMPTZ;


-- 2. Groups
--
-- Replace the legacy workspace/name uniqueness rule with origin-aware
-- uniqueness so SCIM groups can coexist with manual/system groups.

ALTER TABLE groups
    DROP CONSTRAINT IF EXISTS groups_workspace_id_name_key;

ALTER TABLE groups
    ADD COLUMN IF NOT EXISTS origin
        TEXT NOT NULL DEFAULT 'manual'
        CHECK (origin IN ('manual', 'scim', 'system'));

ALTER TABLE groups
    ADD COLUMN IF NOT EXISTS connection_id
        UUID REFERENCES identity_connections(id) ON DELETE CASCADE;

ALTER TABLE groups
    ADD COLUMN IF NOT EXISTS external_id
        TEXT;

CREATE UNIQUE INDEX IF NOT EXISTS idx_groups_manual_name
    ON groups (workspace_id, name)
    WHERE origin IN ('manual', 'system');

CREATE UNIQUE INDEX IF NOT EXISTS idx_groups_scim_external_id
    ON groups (workspace_id, connection_id, external_id)
    WHERE origin = 'scim';

CREATE INDEX IF NOT EXISTS idx_groups_connection
    ON groups (workspace_id, connection_id);


-- 3. Users
--
-- provisioned_by records immutable origin.
-- provisioning_owner records current lifecycle authority.

ALTER TABLE users
    ADD COLUMN IF NOT EXISTS provisioned_by
        TEXT NOT NULL DEFAULT 'jit'
        CHECK (provisioned_by IN ('jit', 'scim', 'manual'));

ALTER TABLE users
    ADD COLUMN IF NOT EXISTS provisioning_owner
        TEXT NOT NULL DEFAULT 'jit'
        CHECK (provisioning_owner IN ('jit', 'manual', 'scim', 'unmanaged'));


-- 4. SCIM synchronization instances
--
-- Created before users/external_identities reference it.

CREATE TABLE IF NOT EXISTS scim_sync_instances (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id  UUID NOT NULL
        REFERENCES workspaces(id) ON DELETE CASCADE,
    connection_id UUID NOT NULL
        REFERENCES identity_connections(id) ON DELETE CASCADE,
    external_id   TEXT,
    display_name  TEXT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_sync_at  TIMESTAMPTZ,

    UNIQUE (workspace_id, connection_id, external_id)
);

ALTER TABLE users
    ADD COLUMN IF NOT EXISTS sync_instance_id
        UUID REFERENCES scim_sync_instances(id) ON DELETE SET NULL;

ALTER TABLE external_identities
    ADD COLUMN IF NOT EXISTS sync_instance_id
        UUID REFERENCES scim_sync_instances(id) ON DELETE SET NULL;


-- 5. SCIM bearer tokens
--
-- The application stores the token hash, never the plaintext token.
-- HMAC-SHA256 generation/validation is Phase 2 application logic.

CREATE TABLE IF NOT EXISTS scim_tokens (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id  UUID NOT NULL
        REFERENCES workspaces(id) ON DELETE CASCADE,
    connection_id UUID NOT NULL
        REFERENCES identity_connections(id) ON DELETE CASCADE,
    token_hash    TEXT NOT NULL,
    label         TEXT,
    created_by    UUID,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_used_at  TIMESTAMPTZ,
    expires_at    TIMESTAMPTZ,
    revoked_at    TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_scim_tokens_hash
    ON scim_tokens (token_hash);

CREATE INDEX IF NOT EXISTS idx_scim_tokens_scope
    ON scim_tokens (workspace_id, connection_id);


-- 6. SCIM identity conflicts
--
-- A pending conflict is unique per workspace/connection/canonical key.
-- The partial unique index is important for concurrent SCIM requests.

CREATE TABLE IF NOT EXISTS scim_identity_conflicts (
    id                     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id           UUID NOT NULL
        REFERENCES workspaces(id) ON DELETE CASCADE,
    connection_id          UUID NOT NULL
        REFERENCES identity_connections(id) ON DELETE CASCADE,
    user_id                UUID NOT NULL
        REFERENCES users(id) ON DELETE CASCADE,
    canonical_identity_key TEXT NOT NULL,
    scim_external_id       TEXT,
    scim_username_snapshot TEXT,
    scim_email_snapshot    TEXT,
    status                 TEXT NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'approved', 'rejected', 'expired')),
    created_at             TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    resolved_at            TIMESTAMPTZ,
    resolved_by            UUID,
    -- The mandatory reason supplied to Accept-Link / Reject (ADR-025 §4.1),
    -- stored alongside the row it explains so the admin queue is self-
    -- describing without cross-referencing audit_logs. The audit log remains
    -- the authoritative history; this is the current-state copy.
    -- Nullable: a conflict returned to 'pending' by Reopen has no resolution.
    resolution_reason      TEXT
);

-- resolution_reason was added after 034 first shipped. The CREATE TABLE above
-- covers fresh databases, but `CREATE TABLE IF NOT EXISTS` is a no-op where the
-- table already exists — so databases that ran the original 034 need this ALTER
-- to pick the column up on re-apply. Same belt-and-braces pattern as the
-- ALTER ... ADD COLUMN IF NOT EXISTS blocks above.
ALTER TABLE scim_identity_conflicts
    ADD COLUMN IF NOT EXISTS resolution_reason TEXT;

CREATE UNIQUE INDEX IF NOT EXISTS idx_conflicts_uniq_pending
    ON scim_identity_conflicts (
        workspace_id,
        connection_id,
        canonical_identity_key
    )
    WHERE status = 'pending';

-- 6. Explicit fine-grained permissions (Phase 3 — break-glass primitive).
--
-- A row means exactly: "user_id explicitly holds `permission` in workspace_id."
-- Possession is NEVER implied by role (an ADMIN without a row does NOT satisfy a
-- permission check). Grant/revoke manage rows; HasPermission answers "does the
-- row exist?". See controller/internal/permission.

CREATE TABLE IF NOT EXISTS workspace_permissions (
    workspace_id UUID NOT NULL
        REFERENCES workspaces(id) ON DELETE CASCADE,
    user_id      UUID NOT NULL
        REFERENCES users(id) ON DELETE CASCADE,
    permission   TEXT NOT NULL,
    granted_by UUID REFERENCES users(id) ON DELETE SET NULL,
    granted_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (workspace_id, user_id, permission)
);