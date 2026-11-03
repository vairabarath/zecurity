-- Connector revocation Phase F (O14): a durable tombstone for revoked
-- connector identities, and no reuse of a revoked identity.
--
-- connector_revocations has no foreign key to connectors: a revoked
-- connector's row can be deleted (DeleteConnector), but its identity (its
-- ID, hence its SPIFFE ID) stays revoked. Revocation is terminal: nothing
-- deletes a tombstone except deleting the whole workspace. Follows
-- shield_revocations (038).
--
-- The tombstone is written by a trigger on connectors, in the same statement
-- (hence the same transaction) as the revocation it records, so every
-- revocation path is covered, including any added later. The revoked
-- predicate is the one connector_certificates uses (039).
--
-- [schema: reset DB] — there is no migration framework in the pre-production
-- phase: recreate the local database after pulling this file
-- (cd controller && docker compose down -v && docker compose up -d).

CREATE TABLE connector_revocations (
    connector_id UUID        PRIMARY KEY,
    tenant_id    UUID        NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    revoked_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_connector_revocations_tenant ON connector_revocations (tenant_id);

CREATE FUNCTION connectors_record_revocation() RETURNS trigger AS $$
BEGIN
    IF NEW.status = 'revoked' OR NEW.revoked_at IS NOT NULL THEN
        INSERT INTO connector_revocations (connector_id, tenant_id, revoked_at)
        VALUES (NEW.id, NEW.tenant_id, COALESCE(NEW.revoked_at, NOW()))
        ON CONFLICT (connector_id) DO NOTHING;
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER connectors_record_revocation
    AFTER INSERT OR UPDATE OF status, revoked_at ON connectors
    FOR EACH ROW EXECUTE FUNCTION connectors_record_revocation();

-- A revoked connector's identity can never be given to a new connector.
CREATE FUNCTION connectors_reject_revoked_identity() RETURNS trigger AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM connector_revocations WHERE connector_id = NEW.id) THEN
        RAISE EXCEPTION 'connector identity % was revoked and cannot be reused', NEW.id
            USING ERRCODE = 'unique_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER connectors_reject_revoked_identity
    BEFORE INSERT OR UPDATE OF id ON connectors
    FOR EACH ROW EXECUTE FUNCTION connectors_reject_revoked_identity();

-- Connectors revoked before this migration get their tombstones: the rows
-- still present, and the revoked-then-deleted ones whose certificates the
-- history (039) recorded as revoked.
INSERT INTO connector_revocations (connector_id, tenant_id, revoked_at)
SELECT id, tenant_id, COALESCE(revoked_at, updated_at)
  FROM connectors
 WHERE status = 'revoked' OR revoked_at IS NOT NULL
ON CONFLICT (connector_id) DO NOTHING;

INSERT INTO connector_revocations (connector_id, tenant_id, revoked_at)
SELECT cc.connector_id, MIN(cc.tenant_id::text)::uuid, MIN(cc.revoked_at)
  FROM connector_certificates cc
 WHERE cc.revoked_at IS NOT NULL
   AND NOT EXISTS (SELECT 1 FROM connectors c WHERE c.id = cc.connector_id)
 GROUP BY cc.connector_id
ON CONFLICT (connector_id) DO NOTHING;
