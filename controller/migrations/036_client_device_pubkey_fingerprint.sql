-- 036_client_device_pubkey_fingerprint.sql
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
-- Renumbered 035 -> 036 at integration time: feat/sprint17-scim (merged via
-- PR #80) claimed both 034_scim_directory_sync.sql and
-- 035_groups_sync_instance.sql first. No schema overlap either way — those
-- touch identity_connections/groups/scim_sync_instances, never client_devices.

ALTER TABLE client_devices
    ADD COLUMN public_key_fingerprint TEXT;
