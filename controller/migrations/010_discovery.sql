-- Shield-local service discovery
CREATE TABLE shield_discovered_services (
  shield_id    UUID    NOT NULL REFERENCES shields(id) ON DELETE CASCADE,
  protocol     TEXT    NOT NULL DEFAULT 'tcp',
  port         INTEGER NOT NULL CHECK (port > 0 AND port < 65536),
  bound_ip     TEXT    NOT NULL,
  service_name TEXT    NOT NULL DEFAULT '',
  first_seen   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  last_seen    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  PRIMARY KEY (shield_id, protocol, port)
);

CREATE INDEX idx_sds_shield_id ON shield_discovered_services (shield_id);

-- Connector network scan results
CREATE TABLE connector_scan_results (
  request_id   TEXT    NOT NULL,
  connector_id UUID    NOT NULL,
  ip           TEXT    NOT NULL,
  port         INTEGER NOT NULL,
  protocol     TEXT    NOT NULL DEFAULT 'tcp',
  service_name TEXT    NOT NULL DEFAULT '',
  first_seen   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  PRIMARY KEY (request_id, ip, port, protocol)
);

CREATE INDEX idx_csr_request_id   ON connector_scan_results (request_id);
CREATE INDEX idx_csr_first_seen   ON connector_scan_results (first_seen);
CREATE INDEX idx_csr_connector_id ON connector_scan_results (connector_id);
