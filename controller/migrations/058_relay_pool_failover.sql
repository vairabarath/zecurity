-- Relay pool failover (Decision Record D-39; relay-pools PR 2).
--
-- A workspace may name a failover pool. The controller sends a workspace's
-- connectors the failover pool's relays while the primary pool is exhausted,
-- and the primary's again once it has recovered for a hold-down period
-- (RELAY_POOL_FAILBACK_HOLDDOWN, default 5 minutes).
--
-- The state lives on the primary pool, in Postgres, so a controller restart
-- resumes mid-failover:
--   exhausted_at   set: the pool has had no eligible relay since then (the
--                  effective pool of its tenants with a failover pool is that
--                  failover pool);
--   eligible_since set while exhausted: the pool has had an eligible relay
--                  continuously since then; after the hold-down both clear.
-- Both stay NULL for pools no workspace uses as primary with a failover, so
-- a new or unused pool is never "recovering".
ALTER TABLE workspaces
    ADD COLUMN relay_failover_pool_id UUID
        REFERENCES relay_pools(id) ON DELETE RESTRICT,
    ADD CONSTRAINT workspaces_relay_failover_differs
        CHECK (relay_failover_pool_id IS DISTINCT FROM relay_pool_id);
CREATE INDEX workspaces_relay_failover_pool_id_idx ON workspaces (relay_failover_pool_id)
    WHERE relay_failover_pool_id IS NOT NULL;

ALTER TABLE relay_pools
    ADD COLUMN exhausted_at   TIMESTAMPTZ,
    ADD COLUMN eligible_since TIMESTAMPTZ,
    ADD CONSTRAINT relay_pools_eligible_since_needs_exhausted
        CHECK (eligible_since IS NULL OR exhausted_at IS NOT NULL);
