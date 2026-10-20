-- Certificate audit bug 9: the workspace CRL covered only the newest
-- certificate of a revoked connector or client device.
--
-- connectors.cert_serial and client_devices.cert_serial hold the current
-- certificate only: every renewal overwrote them, so revoking a renewed
-- connector or device left its earlier, still-valid certificates off the
-- CRL, and deleting a revoked connector dropped it from the CRL entirely.
--
-- These tables keep one row per certificate issued, with its own revocation
-- state, following relay_certificates (027). The workspace CRL is built from
-- them: every revoked certificate stays listed until it expires, whatever
-- happens to the owning row later.
--
-- History is written by triggers on the owning tables, in the same statement
-- (hence the same transaction) as the write it records, so every issuance and
-- revocation path is covered, including any added later:
--   - a new or changed cert_serial records the certificate (issuance);
--   - the first transition to revoked marks every certificate of the owner
--     revoked (revocation is terminal; nothing clears it).
-- A rolled-back issuance or revocation rolls its history back with it.
--
-- No foreign key to the owning row: the history must outlive a deleted
-- connector or device. Only deleting the whole workspace removes it.
--
-- Backfill: only the serial each row currently holds can be recorded.
-- Earlier serials were overwritten before this migration and cannot be
-- recovered; they stay off the CRL until they expire (at most one
-- certificate lifetime after the last renewal before this migration).
--
-- A certificate without a recorded cert_not_after is kept with
-- not_after = 'infinity': if it is ever revoked it stays listed, rather
-- than an unknown expiry letting a revoked certificate drop off the CRL.
--
-- [schema: reset DB] — there is no migration framework in the pre-production
-- phase: recreate the local database after pulling this file
-- (cd controller && docker compose down -v && docker compose up -d).

CREATE TABLE connector_certificates (
    id            UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     UUID        NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    connector_id  UUID        NOT NULL,             -- no FK: outlives the connector row
    serial        TEXT        NOT NULL,             -- canonical SerialNumber.Text(16)
    issued_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    not_after     TIMESTAMPTZ NOT NULL,
    revoked_at    TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX connector_certificates_serial_key ON connector_certificates (serial);
CREATE INDEX connector_certificates_connector_idx ON connector_certificates (connector_id);
CREATE INDEX connector_certificates_crl_idx
    ON connector_certificates (tenant_id, not_after) WHERE revoked_at IS NOT NULL;

CREATE TABLE client_device_certificates (
    id            UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     UUID        NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    device_id     UUID        NOT NULL,             -- no FK: outlives the device row
    serial        TEXT        NOT NULL,             -- canonical SerialNumber.Text(16)
    issued_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    not_after     TIMESTAMPTZ NOT NULL,
    revoked_at    TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX client_device_certificates_serial_key ON client_device_certificates (serial);
CREATE INDEX client_device_certificates_device_idx ON client_device_certificates (device_id);
CREATE INDEX client_device_certificates_crl_idx
    ON client_device_certificates (tenant_id, not_after) WHERE revoked_at IS NOT NULL;

-- Connectors: revoked means status 'revoked' or revoked_at set (either marker
-- on its own, as the controller treats them).
CREATE FUNCTION connectors_certificate_history() RETURNS trigger AS $$
DECLARE
    now_revoked BOOLEAN := NEW.status = 'revoked' OR NEW.revoked_at IS NOT NULL;
    was_revoked BOOLEAN := FALSE;
BEGIN
    IF TG_OP = 'UPDATE' THEN
        was_revoked := OLD.status = 'revoked' OR OLD.revoked_at IS NOT NULL;
    END IF;

    IF NEW.cert_serial IS NOT NULL AND NEW.cert_serial <> ''
       AND (TG_OP = 'INSERT' OR NEW.cert_serial IS DISTINCT FROM OLD.cert_serial) THEN
        INSERT INTO connector_certificates (tenant_id, connector_id, serial, not_after, revoked_at)
        VALUES (NEW.tenant_id, NEW.id, NEW.cert_serial,
                COALESCE(NEW.cert_not_after, 'infinity'),
                CASE WHEN now_revoked THEN COALESCE(NEW.revoked_at, NOW()) END);
    END IF;

    IF now_revoked AND NOT was_revoked THEN
        UPDATE connector_certificates
           SET revoked_at = COALESCE(NEW.revoked_at, NOW())
         WHERE connector_id = NEW.id AND revoked_at IS NULL;
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER connectors_certificate_history
    AFTER INSERT OR UPDATE OF cert_serial, status, revoked_at ON connectors
    FOR EACH ROW EXECUTE FUNCTION connectors_certificate_history();

-- Client devices: revoked means revoked_at set.
CREATE FUNCTION client_devices_certificate_history() RETURNS trigger AS $$
DECLARE
    was_revoked BOOLEAN := FALSE;
BEGIN
    IF TG_OP = 'UPDATE' THEN
        was_revoked := OLD.revoked_at IS NOT NULL;
    END IF;

    IF NEW.cert_serial IS NOT NULL AND NEW.cert_serial <> ''
       AND (TG_OP = 'INSERT' OR NEW.cert_serial IS DISTINCT FROM OLD.cert_serial) THEN
        INSERT INTO client_device_certificates (tenant_id, device_id, serial, not_after, revoked_at)
        VALUES (NEW.workspace_id, NEW.id, NEW.cert_serial,
                COALESCE(NEW.cert_not_after, 'infinity'), NEW.revoked_at);
    END IF;

    IF NEW.revoked_at IS NOT NULL AND NOT was_revoked THEN
        UPDATE client_device_certificates
           SET revoked_at = NEW.revoked_at
         WHERE device_id = NEW.id AND revoked_at IS NULL;
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER client_devices_certificate_history
    AFTER INSERT OR UPDATE OF cert_serial, revoked_at ON client_devices
    FOR EACH ROW EXECUTE FUNCTION client_devices_certificate_history();

-- Backfill the certificates the database still knows about.
INSERT INTO connector_certificates (tenant_id, connector_id, serial, issued_at, not_after, revoked_at)
SELECT tenant_id, id, cert_serial, created_at,
       COALESCE(cert_not_after, 'infinity'),
       CASE WHEN status = 'revoked' OR revoked_at IS NOT NULL
            THEN COALESCE(revoked_at, updated_at) END
  FROM connectors
 WHERE cert_serial IS NOT NULL AND cert_serial <> ''
ON CONFLICT (serial) DO NOTHING;

INSERT INTO client_device_certificates (tenant_id, device_id, serial, issued_at, not_after, revoked_at)
SELECT workspace_id, id, cert_serial, created_at,
       COALESCE(cert_not_after, 'infinity'), revoked_at
  FROM client_devices
 WHERE cert_serial IS NOT NULL AND cert_serial <> ''
ON CONFLICT (serial) DO NOTHING;
