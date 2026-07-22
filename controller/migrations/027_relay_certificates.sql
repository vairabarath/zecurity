-- 027_relay_certificates.sql
-- Per-issued-certificate history for relays so revocation can cover EVERY unexpired
-- serial a relay has held (renewal-safe). relays.cert_serial stays the "current"
-- pointer; this table is the authoritative revocation source.
-- Rows are NEVER hard-deleted: a revoked serial must remain until not_after so it
-- keeps appearing on the relay CRL.

CREATE TABLE relay_certificates (
    id                UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    relay_id          UUID        NOT NULL REFERENCES relays(id),   -- no ON DELETE CASCADE
    serial            TEXT        NOT NULL,                          -- canonical SerialNumber.Text(16)
    issued_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    not_after         TIMESTAMPTZ NOT NULL,
    revoked_at        TIMESTAMPTZ,
    revocation_reason TEXT,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX relay_certificates_serial_key ON relay_certificates (serial);
CREATE INDEX relay_certificates_relay_id_idx ON relay_certificates (relay_id);
CREATE INDEX relay_certificates_revoked_idx
    ON relay_certificates (serial) WHERE revoked_at IS NOT NULL;