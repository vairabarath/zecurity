-- 028_relay_revoked_status.sql
-- Add a terminal 'revoked' status for relays whose certificate has been revoked
-- (PENDING-02). Distinct from 'deleted' so operators can tell a revoked relay
-- (cert killed, kept for the CRL) from a removed one. RecordHeartbeat and
-- MarkProvisioned guard against it; BuildLabelledRelayList (status='active') and
-- the transport snapshot already exclude non-active relays.
ALTER TABLE relays DROP CONSTRAINT relays_status_check;
ALTER TABLE relays
    ADD CONSTRAINT relays_status_check
    CHECK (status IN ('pending', 'active', 'inactive', 'deleted', 'revoked'));
