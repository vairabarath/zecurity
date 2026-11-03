-- Certificate audit bug 10: shield renewal accepted a different key.
--
-- A shield's renewal is proxied by its connector, so the controller never
-- sees the shield's own certificate and had nothing to compare a renewal
-- CSR's key with: it signed whatever key the CSR carried, letting a
-- connector silently re-key its shields.
--
-- public_key_fingerprint pins the key a shield enrolled with: SHA-256 over
-- the key's PKIX (SubjectPublicKeyInfo) DER, lowercase hex, computed from
-- the parsed CSR key exactly as client_devices.public_key_fingerprint (036)
-- and the relay pin are. Enrollment records it in the enrollment
-- transaction; renewal requires the CSR key to match it and never changes
-- it. A different key needs re-enrollment.
--
-- No backfill: the controller never stored shield keys. Existing shields have
-- no pin and must re-enroll before they can renew (no trust on first use).
--
-- [schema: reset DB] — there is no migration framework in the pre-production
-- phase: recreate the local database after pulling this file
-- (cd controller && docker compose down -v && docker compose up -d).

ALTER TABLE shields
    ADD COLUMN public_key_fingerprint TEXT
        CONSTRAINT shields_public_key_fingerprint_format
        CHECK (public_key_fingerprint ~ '^[0-9a-f]{64}$');
