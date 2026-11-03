-- 031_device_profile_manual_trust.sql
-- Sprint 15 M4 Phase 2: track whether a device profile's verification
-- requirements include Manual Trust. Defaults true — Manual Trust is
-- currently the only verification method that exists, so every existing
-- and newly-created profile satisfies it until other trust methods land.

ALTER TABLE device_profiles
    ADD COLUMN manual_trust_enabled BOOLEAN NOT NULL DEFAULT true;
