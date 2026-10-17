-- Certificate audit bug 3: revoking a shield must cut it off, durably.
--
-- shield_revocations is the tombstone. It has no foreign key to shields: the
-- shields row can disappear (DeleteShield, or a cascade from its connector or
-- remote network), but the shield's certificate may still be valid, so the
-- revocation must outlive the row. Revocation is terminal: nothing deletes a
-- tombstone except deleting the whole workspace.
--
-- [schema: reset DB] — there is no migration framework in the pre-production
-- phase: recreate the local database after pulling this file
-- (cd controller && docker compose down -v && docker compose up -d).

CREATE TABLE shield_revocations (
    shield_id   UUID        PRIMARY KEY,
    tenant_id   UUID        NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    revoked_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_shield_revocations_tenant ON shield_revocations (tenant_id);

-- shield_revocation_outbox is the durable propagation work, written in the
-- same transaction as the revocation. The controller's dispatcher drains it:
-- it invalidates the workspace's policy and pushes the workspace's complete
-- revocation list to every connected connector, deleting a row only after
-- that succeeded. A row left behind by a crash or a failed push is retried.
CREATE TABLE shield_revocation_outbox (
    id          BIGSERIAL   PRIMARY KEY,
    tenant_id   UUID        NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    shield_id   UUID        NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    attempts    INT         NOT NULL DEFAULT 0,
    last_error  TEXT
);

CREATE INDEX idx_shield_revocation_outbox_tenant ON shield_revocation_outbox (tenant_id);

-- A revoked shield's identity (its ID, hence its SPIFFE ID) can never be
-- given to a new shield.
CREATE FUNCTION shields_reject_revoked_identity() RETURNS trigger AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM shield_revocations WHERE shield_id = NEW.id) THEN
        RAISE EXCEPTION 'shield identity % was revoked and cannot be reused', NEW.id
            USING ERRCODE = 'unique_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER shields_reject_revoked_identity
    BEFORE INSERT OR UPDATE OF id ON shields
    FOR EACH ROW EXECUTE FUNCTION shields_reject_revoked_identity();

-- Shields revoked before this migration get their tombstones.
INSERT INTO shield_revocations (shield_id, tenant_id, revoked_at)
SELECT id, tenant_id, updated_at FROM shields WHERE status = 'revoked'
ON CONFLICT (shield_id) DO NOTHING;
