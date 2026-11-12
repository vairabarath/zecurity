-- Relay pools (Decision Record D-37, D-38, D-40, D-41; relay-pools PR 1).
--
-- Every relay belongs to exactly one pool, and every workspace has a primary
-- pool. Connectors receive only the eligible relays of their workspace's pool
-- (ADR-016: the controller filters the list, the connector still chooses).
-- The built-in default pool has a fixed id, no region and is never a residency
-- pool; columns default to it, so nothing changes until a provider uses pools.
--
-- region: a validated free-form code (lowercase, hyphen-separated, at most 32
-- characters, e.g. in-south-1). A missing region is never a residency region.
-- residency: the pool is a residency boundary for relay traffic (R2), which
-- requires a region. Residency is only claimed once relay-side enforcement
-- exists (PR 3); until then pools are routing only (R1).
CREATE TABLE relay_pools (
    id         UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    name       TEXT        NOT NULL UNIQUE
               CHECK (char_length(name) BETWEEN 1 AND 64 AND name = btrim(name)),
    region     TEXT
               CHECK (char_length(region) <= 32 AND region ~ '^[a-z][a-z0-9]*(-[a-z0-9]+)*$'),
    residency  BOOLEAN     NOT NULL DEFAULT false,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT relay_pools_residency_needs_region CHECK (NOT residency OR region IS NOT NULL),
    -- The default pool never gets a region or residency.
    CONSTRAINT relay_pools_default_is_plain CHECK (
        id <> '00000000-0000-0000-0000-000000000001'
        OR (region IS NULL AND NOT residency))
);

INSERT INTO relay_pools (id, name) VALUES ('00000000-0000-0000-0000-000000000001', 'default');

ALTER TABLE relays
    ADD COLUMN pool_id UUID NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001'
        REFERENCES relay_pools(id) ON DELETE RESTRICT;
CREATE INDEX relays_pool_id_idx ON relays (pool_id);

-- relay_pool_previous_*: a graceful migration (D-40) keeps the previous pool
-- authorized for residency checks until relay_pool_previous_until.
ALTER TABLE workspaces
    ADD COLUMN relay_pool_id UUID NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001'
        REFERENCES relay_pools(id) ON DELETE RESTRICT,
    ADD COLUMN relay_pool_previous_id UUID
        REFERENCES relay_pools(id) ON DELETE RESTRICT,
    ADD COLUMN relay_pool_previous_until TIMESTAMPTZ,
    ADD CONSTRAINT workspaces_relay_pool_previous_pair
        CHECK ((relay_pool_previous_id IS NULL) = (relay_pool_previous_until IS NULL)),
    ADD CONSTRAINT workspaces_relay_pool_previous_differs
        CHECK (relay_pool_previous_id IS DISTINCT FROM relay_pool_id);
CREATE INDEX workspaces_relay_pool_id_idx ON workspaces (relay_pool_id);
CREATE INDEX workspaces_relay_pool_previous_id_idx ON workspaces (relay_pool_previous_id)
    WHERE relay_pool_previous_id IS NOT NULL;

-- 053 granted the runtime role only the tables that existed then.
GRANT SELECT, INSERT, UPDATE, DELETE ON relay_pools TO ztna_app;
