-- Sprint 9: RDE access logs + device revocation

-- Stores tunnel access events emitted by device_tunnel.rs on the Connector.
-- Queried by the Access Log page (admin UI).
CREATE TABLE connector_logs (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID        NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    connector_id TEXT        NOT NULL,
    message      TEXT        NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_connector_logs_workspace ON connector_logs(workspace_id, created_at DESC);

-- Device revocation: allows admins to revoke a client device cert.
-- Connector's CrlManager picks up the revocation on its next 5-min /ca.crl refresh.
ALTER TABLE client_devices ADD COLUMN revoked_at TIMESTAMPTZ;
