CREATE TABLE resources (
  id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id         UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  remote_network_id UUID NOT NULL REFERENCES remote_networks(id) ON DELETE CASCADE,
  shield_id         UUID NOT NULL REFERENCES shields(id) ON DELETE CASCADE,
  name              TEXT NOT NULL,
  description       TEXT,
  protocol          TEXT NOT NULL DEFAULT 'tcp'
                    CHECK (protocol IN ('tcp','udp','any')),
  host              TEXT NOT NULL,
  port_from         INT  CHECK (port_from BETWEEN 1 AND 65535),
  port_to           INT  CHECK (port_to BETWEEN 1 AND 65535),
  CHECK (port_to >= port_from),
  status            TEXT NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('pending','managing','protecting',
                                      'protected','failed','removing','deleted')),
  error_message     TEXT,
  applied_at        TIMESTAMPTZ,
  last_verified_at  TIMESTAMPTZ,
  deleted_at        TIMESTAMPTZ,
  created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  UNIQUE (shield_id, name)
);

CREATE INDEX idx_resources_shield
  ON resources (shield_id)
  WHERE deleted_at IS NULL;

CREATE INDEX idx_resources_managing
  ON resources (shield_id, status)
  WHERE status IN ('managing','removing') AND deleted_at IS NULL;
