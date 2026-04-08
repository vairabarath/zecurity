-- 002_connector_schema.sql
-- Sprint 2: Connector + Remote Network schema
-- DO NOT modify 001_schema.sql — this is an additive migration.

-- =============================================================================
-- Part 1: Extend workspaces table with trust_domain (SPIFFE identity)
-- =============================================================================
-- Why: Every workspace (tenant) needs a unique SPIFFE trust domain.
-- The trust domain is the root of all SPIFFE identities for that tenant's
-- connectors and agents. Format: "ws-<slug>.zecurity.in"
--
-- Example: workspace slug "acme-corp" → trust domain "ws-acme-corp.zecurity.in"
-- A connector in that workspace gets SPIFFE ID:
--   "spiffe://ws-acme-corp.zecurity.in/connector/<connector-uuid>"

ALTER TABLE workspaces
    ADD COLUMN IF NOT EXISTS trust_domain TEXT UNIQUE;

-- Backfill existing workspaces: derive trust_domain from slug
UPDATE workspaces
   SET trust_domain = 'ws-' || slug || '.zecurity.in'
 WHERE trust_domain IS NULL;

-- Now enforce that every workspace MUST have a trust_domain
ALTER TABLE workspaces
    ALTER COLUMN trust_domain SET NOT NULL;

-- Unique index for fast lookups by trust_domain (used during enrollment)
CREATE UNIQUE INDEX IF NOT EXISTS idx_workspaces_trust_domain
    ON workspaces (trust_domain);

-- =============================================================================
-- Part 2: remote_networks table
-- =============================================================================
-- Why: A tenant can have multiple remote networks (home office, AWS, GCP, etc.).
-- Each remote_network groups connectors that belong to the same physical/logical
-- location. This is the parent entity of connectors.
--
-- Hierarchy: workspace (tenant) → remote_network → connector(s)
--
-- Multi-tenant safety: Every query includes tenant_id WHERE clause.
-- CASCADE delete: If a workspace is deleted, all its remote_networks go too.

CREATE TABLE remote_networks (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID        NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    name        TEXT        NOT NULL,
    location    TEXT        NOT NULL CHECK (location IN ('home','office','aws','gcp','azure','other')),
    status      TEXT        NOT NULL DEFAULT 'active' CHECK (status IN ('active','deleted')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (tenant_id, name)  -- names are unique per tenant, not globally
);

-- Index: queries always filter by tenant_id (multi-tenant scoping)
CREATE INDEX idx_remote_networks_tenant ON remote_networks (tenant_id);

-- =============================================================================
-- Part 3: connectors table
-- =============================================================================
-- Why: Each connector is a daemon running on a remote machine. It enrolls via
-- JWT, gets a SPIFFE certificate, then heartbeats back to the controller.
-- This table tracks the full lifecycle of every connector.
--
-- Status lifecycle: pending → active ↔ disconnected → revoked
--   pending:       Created, token generated, waiting for enrollment
--   active:        Enrolled, heartbeating normally
--   disconnected:  Missed heartbeats (watcher marks it)
--   revoked:       Admin revoked, certificate no longer trusted
--
-- Key columns explained:
--   enrollment_token_jti: JWT "jti" claim — burned after enrollment (traceability)
--   trust_domain:         SPIFFE trust domain assigned at enrollment time
--   cert_serial:          X.509 serial number of the connector's certificate
--   cert_not_after:       Certificate expiry — triggers re-enrollment when close
--   last_heartbeat_at:    Last successful heartbeat timestamp
--   version:              Connector binary version (for update monitoring)
--   hostname:             Machine hostname (informational)
--   public_ip:            Public IP at enrollment time (informational)
--
-- Multi-tenant safety: Every query includes tenant_id WHERE clause.
-- CASCADE delete: If workspace or remote_network is deleted, connectors go too.

CREATE TABLE connectors (
    id                   UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id            UUID        NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    remote_network_id    UUID        NOT NULL REFERENCES remote_networks(id) ON DELETE CASCADE,
    name                 TEXT        NOT NULL,
    status               TEXT        NOT NULL DEFAULT 'pending'
                                     CHECK (status IN ('pending','active','disconnected','revoked')),
    enrollment_token_jti TEXT,                    -- JWT jti — burned after use
    trust_domain         TEXT,                    -- SPIFFE trust domain at enrollment
    cert_serial          TEXT,                    -- X.509 certificate serial
    cert_not_after       TIMESTAMPTZ,             -- Certificate expiry
    last_heartbeat_at    TIMESTAMPTZ,             -- Last heartbeat timestamp
    version              TEXT,                    -- Connector binary version
    hostname             TEXT,                    -- Machine hostname
    public_ip            TEXT,                    -- Public IP at enrollment
    created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Indexes: Every query is tenant-scoped, indexes support all query patterns
CREATE INDEX idx_connectors_tenant         ON connectors (tenant_id);
CREATE INDEX idx_connectors_remote_network ON connectors (remote_network_id, tenant_id);
CREATE INDEX idx_connectors_token_jti      ON connectors (enrollment_token_jti);  -- token burn lookup
CREATE INDEX idx_connectors_trust_domain   ON connectors (trust_domain);           -- SPIFFE identity lookup
