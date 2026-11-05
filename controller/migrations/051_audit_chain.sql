-- 051_audit_chain.sql — PENDING-11 S1a: audit hash chain + D-12 append-only.
--
-- 1. Two hash-chained streams per workspace (P-1, P-2):
--      admin  = audit_logs
--      access = connector_logs
--    A BEFORE INSERT trigger locks the stream's head row, so within a stream
--    `seq` is gap-free and in commit order. It doubles as the export cursor.
--    row_hash = sha256(prev_hash || canonical(row)). The canonical encoding is
--    produced by audit_admin_canonical / audit_access_canonical, which the
--    exporter also calls, so there is one definition of the hashed bytes.
--
--    Lock-order rule: the head lock is always the LAST lock a transaction
--    takes (the audit insert is the last write before commit). The trigger
--    itself takes the workspace FOR KEY SHARE before the head (see
--    audit_chain_lock).
--
-- 2. D-12 (P-3, S-5): role ztna_app (the controller's runtime role) may only
--    INSERT and SELECT the audit tables. Deletes go through SECURITY DEFINER
--    functions owned by the migration owner. LOGIN + password are set by
--    zz_app_role_login.sh (compose only); tests use SET ROLE ztna_app.

-- ── canonical encoding helpers ─────────────────────────────────────────────
-- Each field is `<byte length>:<utf8 bytes>`; NULL is '-' (never '0:').
CREATE FUNCTION audit_field(v TEXT) RETURNS BYTEA
LANGUAGE sql IMMUTABLE PARALLEL SAFE
SET search_path = pg_catalog
AS $$
    SELECT CASE
        WHEN v IS NULL THEN '\x2d'::bytea
        ELSE convert_to(octet_length(v)::text || ':', 'UTF8') || convert_to(v, 'UTF8')
    END
$$;

-- Timestamps are UTC epoch microseconds: independent of TimeZone/DateStyle.
CREATE FUNCTION audit_ts(t TIMESTAMPTZ) RETURNS TEXT
LANGUAGE sql IMMUTABLE PARALLEL SAFE
SET search_path = pg_catalog
AS $$
    SELECT (extract(epoch FROM t) * 1000000)::bigint::text
$$;

CREATE FUNCTION audit_genesis(ws UUID, stream TEXT) RETURNS BYTEA
LANGUAGE sql IMMUTABLE PARALLEL SAFE
SET search_path = pg_catalog
AS $$
    SELECT sha256(convert_to('zecurity-audit-v1:' || ws::text || ':' || stream, 'UTF8'))
$$;

-- ── stream heads ───────────────────────────────────────────────────────────
CREATE TABLE audit_stream_heads (
    workspace_id        UUID   NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    stream              TEXT   NOT NULL CHECK (stream IN ('admin', 'access')),
    last_seq            BIGINT NOT NULL DEFAULT 0,
    last_hash           BYTEA  NOT NULL,
    -- Retention checkpoint (P-4): rows with seq <= purged_through_seq are gone;
    -- purged_through_hash is the row_hash of that last purged row (NULL = none
    -- purged, the chain starts at the genesis hash).
    purged_through_seq  BIGINT NOT NULL DEFAULT 0,
    purged_through_hash BYTEA,
    PRIMARY KEY (workspace_id, stream)
);

-- ── chain columns ──────────────────────────────────────────────────────────
ALTER TABLE audit_logs
    ADD COLUMN seq       BIGINT NOT NULL,
    ADD COLUMN prev_hash BYTEA  NOT NULL,
    ADD COLUMN row_hash  BYTEA  NOT NULL;
CREATE UNIQUE INDEX audit_logs_stream_seq ON audit_logs (tenant_id, seq);

ALTER TABLE connector_logs
    ADD COLUMN seq       BIGINT NOT NULL,
    ADD COLUMN prev_hash BYTEA  NOT NULL,
    ADD COLUMN row_hash  BYTEA  NOT NULL;
CREATE UNIQUE INDEX connector_logs_stream_seq ON connector_logs (workspace_id, seq);

-- ── canonical rows (stream version 'v1'; fields are only ever appended in a
-- new version, never reordered) ───────────────────────────────────────────
CREATE FUNCTION audit_admin_canonical(r audit_logs) RETURNS BYTEA
LANGUAGE sql STABLE
SET search_path = pg_catalog, public
AS $$
    SELECT audit_field('v1')
        || audit_field('admin')
        || audit_field(r.seq::text)
        || audit_field(r.id::text)
        || audit_field(r.tenant_id::text)
        || audit_field(r.actor_user_id::text)
        || audit_field(r.actor_email)
        || audit_field(r.action)
        || audit_field(r.target_type)
        || audit_field(r.target_id)
        || audit_field(r.details::text)
        || audit_field(audit_ts(r.created_at))
$$;

CREATE FUNCTION audit_access_canonical(r connector_logs) RETURNS BYTEA
LANGUAGE sql STABLE
SET search_path = pg_catalog, public
AS $$
    SELECT audit_field('v1')
        || audit_field('access')
        || audit_field(r.seq::text)
        || audit_field(r.id::text)
        || audit_field(r.workspace_id::text)
        || audit_field(r.connector_id)
        || audit_field(r.message)
        || audit_field(r.resource_id::text)
        || audit_field(r.client_spiffe_id)
        || audit_field(r.client_device_id::text)
        || audit_field(r.user_id::text)
        || audit_field(r.route_type)
        || audit_field(r.destination)
        || audit_field(r.port::text)
        || audit_field(r.protocol)
        || audit_field(r.action)
        || audit_field(r.error)
        || audit_field(audit_ts(r.occurred_at))
        || audit_field(audit_ts(r.created_at))
$$;

-- ── head lock + advance ────────────────────────────────────────────────────
-- Returns the locked head (last_seq, last_hash), creating it if needed. Two
-- first inserts can't race: ON CONFLICT DO NOTHING, then SELECT ... FOR UPDATE.
CREATE FUNCTION audit_chain_lock(ws UUID, st TEXT, OUT last_seq BIGINT, OUT last_hash BYTEA)
LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, public
AS $$
BEGIN
    -- Take the FK's workspace lock BEFORE the head lock. The FK check on the
    -- audit row runs after this BEFORE trigger and takes FOR KEY SHARE on the
    -- workspace; taken only then, a transaction holding the workspace FOR
    -- UPDATE and then writing audit would deadlock against a plain insert
    -- (head -> workspace vs workspace -> head). Lock order: workspace, head.
    PERFORM 1 FROM workspaces w WHERE w.id = ws FOR KEY SHARE;
    SELECT h.last_seq, h.last_hash INTO last_seq, last_hash
      FROM audit_stream_heads h
     WHERE h.workspace_id = ws AND h.stream = st
       FOR UPDATE;
    IF NOT FOUND THEN
        INSERT INTO audit_stream_heads (workspace_id, stream, last_hash)
        VALUES (ws, st, audit_genesis(ws, st))
        ON CONFLICT DO NOTHING;
        SELECT h.last_seq, h.last_hash INTO last_seq, last_hash
          FROM audit_stream_heads h
         WHERE h.workspace_id = ws AND h.stream = st
           FOR UPDATE;
    END IF;
END;
$$;

CREATE FUNCTION audit_chain_advance(ws UUID, st TEXT, new_seq BIGINT, new_hash BYTEA) RETURNS VOID
LANGUAGE sql SECURITY DEFINER
SET search_path = pg_catalog, public
AS $$
    UPDATE audit_stream_heads
       SET last_seq = new_seq, last_hash = new_hash
     WHERE workspace_id = ws AND stream = st
$$;

-- ── chain triggers ─────────────────────────────────────────────────────────
-- Named zz_* so they fire after any other BEFORE INSERT trigger: the hash
-- covers the final row. Caller-supplied seq/prev_hash/row_hash are overwritten.
CREATE FUNCTION audit_logs_chain() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, public
AS $$
DECLARE
    head RECORD;
BEGIN
    SELECT * INTO head FROM audit_chain_lock(NEW.tenant_id, 'admin');
    NEW.seq := head.last_seq + 1;
    NEW.prev_hash := head.last_hash;
    NEW.row_hash := sha256(NEW.prev_hash || audit_admin_canonical(NEW));
    PERFORM audit_chain_advance(NEW.tenant_id, 'admin', NEW.seq, NEW.row_hash);
    RETURN NEW;
END;
$$;

CREATE TRIGGER zz_audit_logs_chain
    BEFORE INSERT ON audit_logs
    FOR EACH ROW EXECUTE FUNCTION audit_logs_chain();

CREATE FUNCTION connector_logs_chain() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, public
AS $$
DECLARE
    head RECORD;
BEGIN
    SELECT * INTO head FROM audit_chain_lock(NEW.workspace_id, 'access');
    NEW.seq := head.last_seq + 1;
    NEW.prev_hash := head.last_hash;
    NEW.row_hash := sha256(NEW.prev_hash || audit_access_canonical(NEW));
    PERFORM audit_chain_advance(NEW.workspace_id, 'access', NEW.seq, NEW.row_hash);
    RETURN NEW;
END;
$$;

CREATE TRIGGER zz_connector_logs_chain
    BEFORE INSERT ON connector_logs
    FOR EACH ROW EXECUTE FUNCTION connector_logs_chain();

-- Every workspace gets both head rows when it is created.
CREATE FUNCTION audit_stream_heads_create() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, public
AS $$
BEGIN
    INSERT INTO audit_stream_heads (workspace_id, stream, last_hash)
    VALUES (NEW.id, 'admin',  audit_genesis(NEW.id, 'admin')),
           (NEW.id, 'access', audit_genesis(NEW.id, 'access'))
    ON CONFLICT DO NOTHING;
    RETURN NEW;
END;
$$;

CREATE TRIGGER workspaces_create_audit_stream_heads
    AFTER INSERT ON workspaces
    FOR EACH ROW EXECUTE FUNCTION audit_stream_heads_create();

INSERT INTO audit_stream_heads (workspace_id, stream, last_hash)
SELECT w.id, s.stream, audit_genesis(w.id, s.stream)
  FROM workspaces w CROSS JOIN (VALUES ('admin'), ('access')) AS s(stream)
ON CONFLICT DO NOTHING;

-- ── access-event retention (P-4, S-6) ──────────────────────────────────────
-- Deletes, per workspace, at most p_batch of the OLDEST access events, and
-- only a seq prefix whose rows were all recorded before p_cutoff (it stops at
-- the first newer row). Records the checkpoint on the head first, so the chain
-- stays verifiable from (purged_through_seq, purged_through_hash). Returns the
-- number of rows deleted; the caller repeats until it returns 0.
CREATE FUNCTION audit_purge_access(p_cutoff TIMESTAMPTZ, p_batch INT) RETURNS BIGINT
LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, public
AS $$
DECLARE
    ws        UUID;
    head      RECORD;
    stop_seq  BIGINT;
    upto      BIGINT;
    upto_hash BYTEA;
    n         BIGINT;
    total     BIGINT := 0;
BEGIN
    IF p_batch IS NULL OR p_batch <= 0 THEN
        RAISE EXCEPTION 'audit_purge_access: p_batch must be positive';
    END IF;
    FOR ws IN SELECT workspace_id FROM audit_stream_heads WHERE stream = 'access' LOOP
        SELECT h.last_seq, h.purged_through_seq INTO head
          FROM audit_stream_heads h
         WHERE h.workspace_id = ws AND h.stream = 'access'
           FOR UPDATE;
        SELECT min(c.seq) INTO stop_seq
          FROM connector_logs c
         WHERE c.workspace_id = ws
           AND c.seq >  head.purged_through_seq
           AND c.seq <= head.purged_through_seq + p_batch
           AND c.created_at >= p_cutoff;
        IF stop_seq IS NOT NULL THEN
            upto := stop_seq - 1;
        ELSE
            upto := least(head.purged_through_seq + p_batch, head.last_seq);
        END IF;
        CONTINUE WHEN upto <= head.purged_through_seq;

        SELECT c.row_hash INTO upto_hash
          FROM connector_logs c
         WHERE c.workspace_id = ws AND c.seq = upto;

        DELETE FROM connector_logs c
         WHERE c.workspace_id = ws
           AND c.seq >  head.purged_through_seq
           AND c.seq <= upto;
        GET DIAGNOSTICS n = ROW_COUNT;

        UPDATE audit_stream_heads
           SET purged_through_seq = upto, purged_through_hash = upto_hash
         WHERE workspace_id = ws AND stream = 'access';
        total := total + n;
    END LOOP;
    RETURN total;
END;
$$;

-- ── D-12: the runtime role ─────────────────────────────────────────────────
-- Roles are cluster-wide and DB-backed tests apply this file concurrently in
-- fresh databases on one cluster, so creation tolerates a concurrent creator.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'ztna_app') THEN
        CREATE ROLE ztna_app NOLOGIN;
    END IF;
EXCEPTION WHEN duplicate_object OR unique_violation THEN
    NULL;
END;
$$;

GRANT USAGE ON SCHEMA public TO ztna_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO ztna_app;
GRANT USAGE, SELECT, UPDATE ON ALL SEQUENCES IN SCHEMA public TO ztna_app;
-- PKI reset (pki/service.go resetPKI) truncates the CA tables on an empty platform.
GRANT TRUNCATE ON ca_intermediate, ca_root TO ztna_app;

-- Append-only audit tables: insert and read only (D-12).
REVOKE UPDATE, DELETE, TRUNCATE ON audit_logs, connector_logs, provider_audit_logs FROM ztna_app;
-- Heads are written only by the SECURITY DEFINER functions above.
REVOKE ALL ON audit_stream_heads FROM ztna_app;
GRANT SELECT ON audit_stream_heads TO ztna_app;

-- Tables and sequences created by later schema files get the same defaults.
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO ztna_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    GRANT USAGE, SELECT, UPDATE ON SEQUENCES TO ztna_app;

-- The privileged functions must not be callable by anyone but the owner and
-- the runtime role (purge is invoked by the controller's retention worker).
REVOKE ALL ON FUNCTION audit_chain_lock(UUID, TEXT) FROM PUBLIC;
REVOKE ALL ON FUNCTION audit_chain_advance(UUID, TEXT, BIGINT, BYTEA) FROM PUBLIC;
REVOKE ALL ON FUNCTION audit_purge_access(TIMESTAMPTZ, INT) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION audit_purge_access(TIMESTAMPTZ, INT) TO ztna_app;
