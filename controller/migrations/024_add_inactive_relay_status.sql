-- F4: allow relays to move active -> inactive on heartbeat expiry,
-- and inactive -> active again when heartbeat returns.

ALTER TABLE relays DROP CONSTRAINT relays_status_check;

ALTER TABLE relays
ADD CONSTRAINT relays_status_check
CHECK (status IN ('pending', 'active', 'inactive', 'deleted'));
