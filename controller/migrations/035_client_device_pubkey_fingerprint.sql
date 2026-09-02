-- 035_client_device_pubkey_fingerprint.sql
-- Sprint 19 Track 3 (PENDING-13, ADR-028 D1/D4): pin the device's public key
-- at enrollment so RenewCert can prove a renewal request actually comes from
-- the device that holds that key, not just someone holding a stolen
-- access_token. Set exactly once, by EnrollDevice; RenewCert only ever
-- compares against it, never overwrites it (see Track3-Renew-Reenroll.md D-A).
--
-- Nullable, no backfill: devices enrolled before this migration have no
-- fingerprint on file. Track 3 treats that as "must re-enroll" rather than
-- trust-on-first-use (D-A) — deliberately no migration-time fallback value.
--
-- Migration number coordination: same caveat as 034_device_status.sql —
-- confirm this number is still free relative to any other branch at
-- integration/rebase time.

ALTER TABLE client_devices
    ADD COLUMN public_key_fingerprint TEXT;
