-- Relay observed/public address metadata, refreshed from authenticated
-- Relay heartbeat peer information.
ALTER TABLE relays
  ADD COLUMN public_addr TEXT,
  ADD COLUMN observed_ip INET,
  ADD COLUMN observed_port INTEGER,
  ADD COLUMN address_scope TEXT CHECK (
    address_scope IN ('public', 'private', 'loopback', 'link_local', 'unknown')
  );
