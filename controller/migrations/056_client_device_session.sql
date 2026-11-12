-- 056_client_device_session.sql — PENDING-09 C-1 (D-1, D-2): a client
-- device's live tunnels end when its login session ends.
--
-- Each device records the login session (sid) it was enrolled under and that
-- session's deadlines. The deadlines live here, in Postgres, not only in
-- Valkey: a device is never revoked because a Valkey key is missing, so a
-- Valkey flush or restart cannot revoke every device.
--
--   session_id          the refresh session's sid (stable across rotation)
--   session_idle_until  moved forward on every successful refresh of that
--                       session (now + JWT_REFRESH_TTL)
--   session_max_until   the session's absolute lifetime cap (ADR-006,
--                       fixed at the first sign-in)
--
-- NULL session_id: a device enrolled before this column existed; it is never
-- swept (pre-production: none exist after a DB reset).
--
-- Privileges: 053 grants ztna_app table-level DML on client_devices, which
-- covers new columns; nothing more is needed.

ALTER TABLE client_devices
    ADD COLUMN session_id         TEXT,
    ADD COLUMN session_idle_until TIMESTAMPTZ,
    ADD COLUMN session_max_until  TIMESTAMPTZ;

-- Explicit session ends revoke by (user, sid).
CREATE INDEX client_devices_active_session_idx
    ON client_devices (user_id, session_id)
    WHERE revoked_at IS NULL AND session_id IS NOT NULL;

-- The expiry sweeper scans active devices by deadline.
CREATE INDEX client_devices_session_idle_idx
    ON client_devices (session_idle_until)
    WHERE revoked_at IS NULL AND session_id IS NOT NULL;
CREATE INDEX client_devices_session_max_idx
    ON client_devices (session_max_until)
    WHERE revoked_at IS NULL AND session_id IS NOT NULL;
