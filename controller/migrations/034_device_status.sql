-- 034_device_status.sql
-- Sprint 19 Track 2 (PENDING-13): recoverable device states the server signals
-- to the client daemon on the 60s ACL poll. 'revoked' is deliberately NOT a
-- value here — it stays derived from the existing client_devices.revoked_at
-- (single writer of "revoked-ness", no dual-write). Only the re-enroll outbox
-- handler sets 're_enroll_required'; 'renew_pending' is reserved for Track 3.
--
-- Migration number coordination: collides with feat/sprint17-scim's
-- 034_scim_directory_sync.sql (which also takes 035). Whichever branch merges
-- second must renumber at integration/rebase time.

ALTER TABLE client_devices
    ADD COLUMN status TEXT NOT NULL DEFAULT 'active'
        CHECK (status IN ('active', 're_enroll_required', 'renew_pending'));
