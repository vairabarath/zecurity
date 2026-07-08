-- Connector-routed resources do not require a Shield. Protection remains a
-- lifecycle concern: unprotected resources route through the Connector, while
-- protected-intent states route through their Shield.

ALTER TABLE resources
  DROP CONSTRAINT resources_shield_id_fkey;

ALTER TABLE resources
  ALTER COLUMN shield_id DROP NOT NULL;

ALTER TABLE resources
  ADD CONSTRAINT resources_shield_id_fkey
  FOREIGN KEY (shield_id) REFERENCES shields(id) ON DELETE SET NULL;

ALTER TABLE resources
  DROP CONSTRAINT resources_shield_id_name_key;

ALTER TABLE resources
  ADD CONSTRAINT resources_workspace_network_host_name_key
  UNIQUE (tenant_id, remote_network_id, host, name);

-- Pending was the old initial state. New resources enter the terminal
-- unprotected state and are immediately eligible for Connector routing.
UPDATE resources
SET status = 'unprotected',
    pending_action = 'remove',
    updated_at = NOW()
WHERE status = 'pending';

ALTER TABLE connector_logs
  ADD COLUMN resource_id UUID,
  ADD COLUMN client_spiffe_id TEXT,
  ADD COLUMN client_device_id UUID,
  ADD COLUMN user_id UUID,
  ADD COLUMN route_type TEXT CHECK (route_type IN ('connector', 'shield')),
  ADD COLUMN destination TEXT,
  ADD COLUMN port INT CHECK (port BETWEEN 1 AND 65535),
  ADD COLUMN protocol TEXT CHECK (protocol IN ('tcp', 'udp')),
  ADD COLUMN action TEXT CHECK (action IN ('allow', 'deny', 'error')),
  ADD COLUMN error TEXT,
  ADD COLUMN occurred_at TIMESTAMPTZ;

CREATE INDEX idx_connector_logs_user
  ON connector_logs (workspace_id, user_id, occurred_at DESC);

CREATE INDEX idx_connector_logs_resource
  ON connector_logs (workspace_id, resource_id, occurred_at DESC);
