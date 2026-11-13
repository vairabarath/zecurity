-- 057_device_profile_platforms.sql — ADR-031 per-platform device profiles.
--
-- A device profile now lists the platforms it allows. A device whose
-- platform (client_devices.os, fixed at enrollment) is not listed fails the
-- profile; a device is checked only against its own platform's requirements
-- plus the server-observed ones (zecurity.*).
--
-- Existing profiles keep their behaviour (ADR-031 D-3):
--   * with at least one requirement: {linux}. Their checks are all linux.*,
--     which every other platform failed as unsupported; only Linux devices
--     have ever stored a posture report (verified 2026-11-13).
--   * with no requirement: {} (none). They always failed ("profile has no
--     requirements"); {linux} would have opened them.
-- New profiles start with {}: the admin turns platforms on.
--
-- Privileges: ztna_app already has table-level DML on device_profiles.

ALTER TABLE device_profiles
    ADD COLUMN allowed_platforms TEXT[] NOT NULL DEFAULT '{}'
        CONSTRAINT device_profiles_allowed_platforms_known
        CHECK (allowed_platforms <@ ARRAY['linux', 'android', 'windows', 'ios']::TEXT[]);

UPDATE device_profiles p
   SET allowed_platforms = ARRAY['linux']::TEXT[]
 WHERE EXISTS (SELECT 1 FROM device_profile_requirements r WHERE r.profile_id = p.id);
