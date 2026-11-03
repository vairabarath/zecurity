-- PENDING-02: connector certificate revocation.
-- A revoked connector's serial is published on the workspace CRL
-- (GenerateClientCRL) so BOTH the relay outer mTLS (Track-2 WorkspaceCrlManager)
-- and the connector inner mTLS reject it. The 'revoked' status already exists in
-- the connectors CHECK constraint (migration 002). Rows are never deleted — the
-- CRL needs the serial until the certificate expires.
ALTER TABLE connectors ADD COLUMN IF NOT EXISTS revoked_at        TIMESTAMPTZ;
ALTER TABLE connectors ADD COLUMN IF NOT EXISTS revocation_reason TEXT;

CREATE INDEX IF NOT EXISTS idx_connectors_revoked
    ON connectors (tenant_id)
    WHERE revoked_at IS NOT NULL;
