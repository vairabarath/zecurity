-- 003_shield_schema.sql — Shield table for Sprint 4

CREATE TABLE shields (
    id                  UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id           UUID        NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    remote_network_id   UUID        NOT NULL REFERENCES remote_networks(id) ON DELETE CASCADE,
    connector_id        UUID        NOT NULL REFERENCES connectors(id) ON DELETE CASCADE,
    name                TEXT        NOT NULL,
    status              TEXT        NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','active','disconnected','revoked')),
    enrollment_token_jti TEXT,
    trust_domain        TEXT,
    interface_addr      TEXT,
    cert_serial         TEXT,
    cert_not_after      TIMESTAMPTZ,
    last_heartbeat_at   TIMESTAMPTZ,
    version             TEXT,
    hostname            TEXT,
    public_ip           TEXT,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_shields_tenant         ON shields (tenant_id);
CREATE INDEX idx_shields_remote_network ON shields (remote_network_id, tenant_id);
CREATE INDEX idx_shields_connector      ON shields (connector_id);
CREATE INDEX idx_shields_token_jti      ON shields (enrollment_token_jti);
CREATE INDEX idx_shields_trust_domain   ON shields (trust_domain);
CREATE UNIQUE INDEX idx_shields_interface_addr ON shields (tenant_id, interface_addr) WHERE interface_addr IS NOT NULL;
