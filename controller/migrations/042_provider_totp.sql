-- Sprint 22 Phase T: mandatory super-admin TOTP (Decision Record D-28, D-33;
-- amendment 2026-10-26, OQ-1…OQ-3).
--
-- The secret is sealed with pki.EncryptSecret (AES-256-GCM + HKDF under
-- PKI_MASTER_SECRET, context "provider-totp:<user id>"); only ciphertext and
-- nonce are stored. A pending enrolment has a secret but no totp_enabled_at.
-- totp_last_step is the last accepted RFC 6238 time step: a code for that step
-- or an older one is a replay and is refused.
--
-- [schema: reset DB] — no migration framework; recreate local databases.

ALTER TABLE provider_users
    ADD COLUMN totp_secret_ciphertext TEXT,
    ADD COLUMN totp_secret_nonce      TEXT,
    ADD COLUMN totp_enabled_at        TIMESTAMPTZ,
    ADD COLUMN totp_last_step         BIGINT;
