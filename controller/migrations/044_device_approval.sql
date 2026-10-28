-- 044_device_approval.sql (ADR-030)
--
-- An admin approves a client device once ("this is a company machine"). A
-- device profile can then require the server-side posture check
-- zecurity.device.approved, so a resource bound to it admits only approved
-- devices while other resources stay open to BYOD. New devices start
-- unapproved. 043_ stays reserved for Sprint 22 Phase D (043_relay_drain.sql).

ALTER TABLE client_devices
    ADD COLUMN approved_at TIMESTAMPTZ,
    ADD COLUMN approved_by UUID REFERENCES users(id) ON DELETE SET NULL;
