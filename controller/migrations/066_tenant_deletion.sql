-- 066_tenant_deletion.sql — Sprint 23 S4: tenant deletion (F7; Decision
-- Record D-46, implementing D-09, D-10, D-13 and amending D-11; the export
-- and download design of 2026-11-18 is a proposed amendment to D-45/D-46).
--
-- 1. Deletion state on workspaces: a deletion is scheduled on a SUSPENDED
--    tenant only (D-10; the CHECK enforces it), runs after the grace
--    (delete_after), and can be cancelled until then. A legal hold pauses
--    the purge until cleared.
-- 2. tenant_purges: the purge's progress, one row per tenant, so an
--    interrupted purge resumes from its last completed step. It has no FK
--    to workspaces: it outlives the workspace row until the final step.
-- 3. workspace_tombstones: the minimal record left after destruction (D-09):
--    id, slug, actor, timestamps, and where the export went (location and
--    hash only, never content).
-- 4. tenant_export_tokens: download tokens for the tenant's own logs during
--    the grace (emailed link, export-only sign-in). Only the SHA-256 of a
--    token is stored.
-- 5. tenant_purge(ws): the privileged purge path (D-13). The runtime role
--    may not delete workspaces (053_), and the audit tables are append-only
--    for it (D-12); this SECURITY DEFINER function, owned by the migration
--    owner, is the only way a tenant's audit_logs are deleted, and only
--    after a verified export. provider_audit_logs is never touched.
--
-- Pre-production rule: applied only when the Postgres volume is created.
-- [schema: reset DB]

ALTER TABLE workspaces
    ADD COLUMN delete_scheduled_at TIMESTAMPTZ,
    ADD COLUMN delete_after        TIMESTAMPTZ,
    ADD COLUMN delete_reason       TEXT CHECK (delete_reason IS NULL OR char_length(delete_reason) BETWEEN 1 AND 500),
    ADD COLUMN delete_by           TEXT,
    ADD COLUMN legal_hold          BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN legal_hold_reason   TEXT CHECK (legal_hold_reason IS NULL OR char_length(legal_hold_reason) BETWEEN 1 AND 500),
    ADD COLUMN legal_hold_by       TEXT,
    ADD COLUMN legal_hold_at       TIMESTAMPTZ,
    -- The four deletion columns are set and cleared together.
    ADD CONSTRAINT workspaces_delete_columns_together CHECK (
        (delete_scheduled_at IS NULL) = (delete_after IS NULL)
        AND (delete_after IS NULL) = (delete_reason IS NULL)
        AND (delete_reason IS NULL) = (delete_by IS NULL)),
    -- D-10: only a suspended tenant can have a deletion scheduled.
    ADD CONSTRAINT workspaces_delete_only_when_suspended CHECK (
        delete_scheduled_at IS NULL OR suspended_at IS NOT NULL),
    ADD CONSTRAINT workspaces_legal_hold_columns CHECK (
        legal_hold = (legal_hold_reason IS NOT NULL)
        AND legal_hold = (legal_hold_by IS NOT NULL)
        AND legal_hold = (legal_hold_at IS NOT NULL));

-- The purge job's scan: scheduled deletions past their grace.
CREATE INDEX workspaces_delete_due ON workspaces (delete_after) WHERE delete_after IS NOT NULL;

CREATE TABLE tenant_purges (
    workspace_id       UUID        PRIMARY KEY,
    slug               TEXT        NOT NULL,
    -- pending → exported → revoked → destroyed → done
    step               TEXT        NOT NULL DEFAULT 'pending'
                                   CHECK (step IN ('pending', 'exported', 'revoked', 'destroyed', 'done')),
    export_kind        TEXT        CHECK (export_kind IN ('s3', 'local')),
    export_location    TEXT,       -- bucket/key or file path; never content
    export_sha256      TEXT        CHECK (export_sha256 IS NULL OR export_sha256 ~ '^[0-9a-f]{64}$'),
    export_verified_at TIMESTAMPTZ,
    delete_by          TEXT        NOT NULL,
    delete_reason      TEXT        NOT NULL,
    scheduled_at       TIMESTAMPTZ NOT NULL,
    started_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    attempts           INT         NOT NULL DEFAULT 0,
    last_error         TEXT,
    -- A step past 'pending' needs a verified export.
    CHECK (step = 'pending' OR (export_kind IS NOT NULL AND export_location IS NOT NULL
                                AND export_sha256 IS NOT NULL AND export_verified_at IS NOT NULL))
);

CREATE TABLE workspace_tombstones (
    workspace_id    UUID        PRIMARY KEY,
    slug            TEXT        NOT NULL,
    deleted_by      TEXT        NOT NULL,
    delete_reason   TEXT        NOT NULL,
    scheduled_at    TIMESTAMPTZ NOT NULL,
    destroyed_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    export_kind     TEXT        NOT NULL CHECK (export_kind IN ('s3', 'local')),
    export_location TEXT        NOT NULL,
    export_sha256   TEXT        NOT NULL CHECK (export_sha256 ~ '^[0-9a-f]{64}$')
);

CREATE TABLE tenant_export_tokens (
    token_sha256 BYTEA       PRIMARY KEY CHECK (octet_length(token_sha256) = 32),
    workspace_id UUID        NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    purpose      TEXT        NOT NULL CHECK (purpose IN ('email', 'signin')),
    -- The admin it was issued to (email link recipient or the signed-in admin).
    user_id      UUID        NOT NULL,
    expires_at   TIMESTAMPTZ NOT NULL,
    used_at      TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (user_id, workspace_id) REFERENCES users (id, tenant_id) ON DELETE CASCADE
);
CREATE INDEX tenant_export_tokens_workspace ON tenant_export_tokens (workspace_id);

-- ── D-13: the privileged purge path ─────────────────────────────────────────
-- Deletes one tenant: its outbox rows (RESTRICT FK), then the workspace row;
-- every tenant table cascades, including audit_logs, connector_logs and the
-- audit stream heads. Refuses unless the purge row records a verified export
-- and the revoke step, the grace has passed, and the tenant is still
-- suspended, scheduled and not on legal hold. Idempotent: a workspace that is
-- already gone is a no-op (the job then writes the tombstone). Raises with
-- insufficient_privilege on a refused precondition.
CREATE FUNCTION tenant_purge(p_workspace UUID) RETURNS BOOLEAN
LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, public
AS $$
DECLARE
    p RECORD;
    w RECORD;
BEGIN
    SELECT * INTO p FROM tenant_purges WHERE workspace_id = p_workspace FOR UPDATE;
    IF NOT FOUND OR p.step NOT IN ('revoked', 'destroyed') OR p.export_verified_at IS NULL THEN
        RAISE EXCEPTION 'tenant_purge: % has no verified export and revoke step', p_workspace
            USING ERRCODE = 'insufficient_privilege';
    END IF;

    SELECT id, suspended_at, delete_after, legal_hold INTO w
      FROM workspaces WHERE id = p_workspace FOR UPDATE;
    IF NOT FOUND THEN
        RETURN false; -- already destroyed (a resumed purge)
    END IF;
    IF w.suspended_at IS NULL OR w.delete_after IS NULL OR w.delete_after > now() OR w.legal_hold THEN
        RAISE EXCEPTION 'tenant_purge: % is not due (suspended, scheduled, past grace, no legal hold)', p_workspace
            USING ERRCODE = 'insufficient_privilege';
    END IF;

    DELETE FROM outbox_events WHERE workspace_id = p_workspace;
    DELETE FROM workspaces WHERE id = p_workspace;
    RETURN true;
END;
$$;

REVOKE ALL ON FUNCTION tenant_purge(UUID) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION tenant_purge(UUID) TO ztna_app;

-- The runtime role reads and advances purge progress and writes tombstones;
-- it may never delete a tombstone.
GRANT SELECT, INSERT, UPDATE ON tenant_purges, workspace_tombstones, tenant_export_tokens TO ztna_app;
GRANT DELETE ON tenant_purges, tenant_export_tokens TO ztna_app;
REVOKE DELETE, TRUNCATE ON workspace_tombstones FROM ztna_app;
