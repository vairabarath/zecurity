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
-- The runtime role (ztna_app) can't write tenant_purges or the tombstones
-- (only read them): every step change goes through one of the SECURITY
-- DEFINER functions below, owned by the migration owner, and each checks its
-- own preconditions under the workspace row lock. The lock also serialises a
-- step with CancelDelete / SetLegalHold, which lock the same row. Residual:
-- the export is performed by the controller, so tenant_purge_record_export
-- trusts the (kind, location, sha256) it is given; the function cannot read
-- the tenant's bucket or the controller's disk.

-- tenant_due locks the workspace row and reports whether its deletion is due
-- (suspended, scheduled, grace passed, no legal hold). NULL: no such row.
CREATE FUNCTION tenant_due(p_workspace UUID) RETURNS BOOLEAN
LANGUAGE plpgsql
SET search_path = pg_catalog, public
AS $$
DECLARE
    w RECORD;
BEGIN
    SELECT suspended_at, delete_after, legal_hold INTO w FROM workspaces WHERE id = p_workspace FOR UPDATE;
    IF NOT FOUND THEN
        RETURN NULL;
    END IF;
    RETURN w.suspended_at IS NOT NULL AND w.delete_after IS NOT NULL AND w.delete_after <= now() AND NOT w.legal_hold;
END;
$$;

-- Starts the purge of a due tenant (idempotent). Returns whether a purge row
-- exists afterwards.
CREATE FUNCTION tenant_purge_start(p_workspace UUID) RETURNS BOOLEAN
LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, public
AS $$
BEGIN
    IF tenant_due(p_workspace) THEN
        INSERT INTO tenant_purges (workspace_id, slug, delete_by, delete_reason, scheduled_at)
        SELECT id, slug, delete_by, delete_reason, delete_scheduled_at FROM workspaces WHERE id = p_workspace
        ON CONFLICT (workspace_id) DO NOTHING;
    END IF;
    RETURN EXISTS (SELECT 1 FROM tenant_purges WHERE workspace_id = p_workspace);
END;
$$;

-- Records the verified export: only from step 'pending', while due.
CREATE FUNCTION tenant_purge_record_export(p_workspace UUID, p_kind TEXT, p_location TEXT, p_sha256 TEXT) RETURNS VOID
LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, public
AS $$
BEGIN
    IF NOT coalesce(tenant_due(p_workspace), false) THEN
        RAISE EXCEPTION 'tenant_purge_record_export: % is not due', p_workspace USING ERRCODE = 'insufficient_privilege';
    END IF;
    UPDATE tenant_purges
       SET step = 'exported', export_kind = p_kind, export_location = p_location, export_sha256 = p_sha256,
           export_verified_at = now(), last_error = NULL, updated_at = now()
     WHERE workspace_id = p_workspace AND step = 'pending';
    IF NOT FOUND THEN
        RAISE EXCEPTION 'tenant_purge_record_export: % is not at step pending', p_workspace USING ERRCODE = 'insufficient_privilege';
    END IF;
END;
$$;

-- Records the revoke step: only from 'exported', while due, and only when
-- nothing of the tenant can still authenticate (every connector and shield
-- revoked, every client device revoked).
CREATE FUNCTION tenant_purge_mark_revoked(p_workspace UUID) RETURNS VOID
LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, public
AS $$
BEGIN
    IF NOT coalesce(tenant_due(p_workspace), false) THEN
        RAISE EXCEPTION 'tenant_purge_mark_revoked: % is not due', p_workspace USING ERRCODE = 'insufficient_privilege';
    END IF;
    IF EXISTS (SELECT 1 FROM connectors WHERE tenant_id = p_workspace AND status <> 'revoked')
       OR EXISTS (SELECT 1 FROM shields WHERE tenant_id = p_workspace AND status <> 'revoked')
       OR EXISTS (SELECT 1 FROM client_devices WHERE workspace_id = p_workspace AND revoked_at IS NULL) THEN
        RAISE EXCEPTION 'tenant_purge_mark_revoked: % still has unrevoked agents or devices', p_workspace
            USING ERRCODE = 'insufficient_privilege';
    END IF;
    UPDATE tenant_purges SET step = 'revoked', last_error = NULL, updated_at = now()
     WHERE workspace_id = p_workspace AND step = 'exported';
    IF NOT FOUND THEN
        RAISE EXCEPTION 'tenant_purge_mark_revoked: % is not at step exported', p_workspace USING ERRCODE = 'insufficient_privilege';
    END IF;
END;
$$;

-- tenant_purge deletes one tenant: its outbox rows (RESTRICT FK), then the
-- workspace row; every tenant table cascades, including audit_logs,
-- connector_logs and the audit stream heads. Only from step 'revoked' (a
-- verified export recorded), while due; records step 'destroyed'. Idempotent:
-- at step 'destroyed' it is a no-op. provider_audit_logs is never touched.
CREATE FUNCTION tenant_purge(p_workspace UUID) RETURNS BOOLEAN
LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, public
AS $$
DECLARE
    p RECORD;
BEGIN
    SELECT step, export_verified_at INTO p FROM tenant_purges WHERE workspace_id = p_workspace FOR UPDATE;
    IF FOUND AND p.step = 'destroyed' THEN
        RETURN false;
    END IF;
    IF NOT FOUND OR p.step <> 'revoked' OR p.export_verified_at IS NULL THEN
        RAISE EXCEPTION 'tenant_purge: % has no verified export and revoke step', p_workspace
            USING ERRCODE = 'insufficient_privilege';
    END IF;
    IF NOT coalesce(tenant_due(p_workspace), false) THEN
        RAISE EXCEPTION 'tenant_purge: % is not due (suspended, scheduled, past grace, no legal hold)', p_workspace
            USING ERRCODE = 'insufficient_privilege';
    END IF;
    DELETE FROM outbox_events WHERE workspace_id = p_workspace;
    DELETE FROM workspaces WHERE id = p_workspace;
    UPDATE tenant_purges SET step = 'destroyed', last_error = NULL, updated_at = now() WHERE workspace_id = p_workspace;
    RETURN true;
END;
$$;

-- Writes the tombstone from the purge row and records 'done'. Returns
-- whether the tombstone was written now (the caller then writes the
-- provider audit row in the same transaction).
CREATE FUNCTION tenant_purge_finish(p_workspace UUID) RETURNS BOOLEAN
LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, public
AS $$
DECLARE
    n INT;
BEGIN
    PERFORM 1 FROM tenant_purges WHERE workspace_id = p_workspace AND step = 'destroyed' FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'tenant_purge_finish: % is not at step destroyed', p_workspace USING ERRCODE = 'insufficient_privilege';
    END IF;
    INSERT INTO workspace_tombstones (workspace_id, slug, deleted_by, delete_reason, scheduled_at,
                                      export_kind, export_location, export_sha256)
    SELECT workspace_id, slug, delete_by, delete_reason, scheduled_at, export_kind, export_location, export_sha256
      FROM tenant_purges WHERE workspace_id = p_workspace
    ON CONFLICT (workspace_id) DO NOTHING;
    GET DIAGNOSTICS n = ROW_COUNT;
    UPDATE tenant_purges SET step = 'done', last_error = NULL, updated_at = now() WHERE workspace_id = p_workspace;
    RETURN n = 1;
END;
$$;

-- Records a failed attempt (no step change).
CREATE FUNCTION tenant_purge_record_error(p_workspace UUID, p_error TEXT) RETURNS VOID
LANGUAGE sql SECURITY DEFINER
SET search_path = pg_catalog, public
AS $$
    UPDATE tenant_purges SET attempts = attempts + 1, last_error = left(p_error, 500), updated_at = now()
     WHERE workspace_id = p_workspace;
$$;

REVOKE ALL ON FUNCTION tenant_due(UUID), tenant_purge_start(UUID), tenant_purge_record_export(UUID, TEXT, TEXT, TEXT),
    tenant_purge_mark_revoked(UUID), tenant_purge(UUID), tenant_purge_finish(UUID), tenant_purge_record_error(UUID, TEXT) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION tenant_purge_start(UUID), tenant_purge_record_export(UUID, TEXT, TEXT, TEXT),
    tenant_purge_mark_revoked(UUID), tenant_purge(UUID), tenant_purge_finish(UUID), tenant_purge_record_error(UUID, TEXT) TO ztna_app;

-- The runtime role reads purge progress and tombstones only; download tokens
-- it manages directly.
REVOKE ALL ON tenant_purges, workspace_tombstones FROM ztna_app;
GRANT SELECT ON tenant_purges, workspace_tombstones TO ztna_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON tenant_export_tokens TO ztna_app;
