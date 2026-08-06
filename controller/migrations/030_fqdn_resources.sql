-- PENDING-14 Stage 2 (Sprint 16 Phase 4): FQDN-addressable resources.
--
-- Until now a resource WAS an IP: `host` was NOT NULL and the client keyed its
-- transports on it. A resource that is only reachable by name, or whose backend IP
-- moves (cloud DNS, load balancers, k8s services), could not be expressed at all.
--
-- This migration separates the three concepts that `host` was conflating:
--   hostname     — the CLIENT-FACING name (what an app/DNS/TLS uses). Independent
--                  of delivery; a resource may have a name AND a pinned IP.
--   host         — a pinned IP. Now NULLABLE: FQDN resources have no fixed address;
--                  the connector resolves the endpoint per connection instead.
--   resolver     — HOW the connector finds the current endpoint, for
--                  connector-reachable resources: {"type":"dns"|"static", ...}.
--   local_target — for shield-delivered resources, which local address the shield
--                  dials (127.0.0.1 vs its LAN IP).
--
-- `route_type` is NOT stored: it stays derived from (status, shield_id) in
-- policy.routeTypeForResource. Nothing here changes that.
--
-- Safe/additive: every new column is nullable, so existing IP resources keep
-- working untouched and the ACL compiles identically for them.

-- ── New addressing columns ──────────────────────────────────────────────────

ALTER TABLE resources ADD COLUMN IF NOT EXISTS hostname     TEXT;
ALTER TABLE resources ADD COLUMN IF NOT EXISTS resolver     JSONB;
ALTER TABLE resources ADD COLUMN IF NOT EXISTS local_target TEXT;

-- FQDN resources carry no pinned IP.
ALTER TABLE resources ALTER COLUMN host DROP NOT NULL;

-- A resource must still be addressable *somehow* — a row with neither an IP nor a
-- name is meaningless and would compile into an unroutable ACL entry.
ALTER TABLE resources DROP CONSTRAINT IF EXISTS resources_addressable_check;
ALTER TABLE resources ADD CONSTRAINT resources_addressable_check
  CHECK (host IS NOT NULL OR hostname IS NOT NULL);

-- Resolver config, when present, must at least declare its type; the connector
-- dispatches on it and a typeless blob would fail closed at dial time.
ALTER TABLE resources DROP CONSTRAINT IF EXISTS resources_resolver_shape_check;
ALTER TABLE resources ADD CONSTRAINT resources_resolver_shape_check
  CHECK (resolver IS NULL OR (jsonb_typeof(resolver) = 'object' AND resolver ? 'type'));

-- ── Uniqueness now that `host` can be NULL ──────────────────────────────────
--
-- The old UNIQUE (tenant_id, remote_network_id, host, name) silently stops
-- enforcing anything once host IS NULL, because Postgres treats NULLs as distinct
-- — two FQDN resources could share a name in the same network. Key on the
-- effective address instead, preserving the previous semantics for IP resources.

ALTER TABLE resources DROP CONSTRAINT IF EXISTS resources_workspace_network_host_name_key;

CREATE UNIQUE INDEX IF NOT EXISTS resources_workspace_network_addr_name_key
  ON resources (tenant_id, remote_network_id, COALESCE(host, hostname), name);

-- ── Shield HA: many shields may serve one resource ──────────────────────────
--
-- `resources.shield_id` (singular) cannot express a service replicated across
-- hosts, each running a shield. Introduced as a join table now because HA is
-- expensive to retrofit later. `resources.shield_id` is KEPT and still authoritative
-- for existing readers (policy compiler, reconciler); this table is populated in
-- parallel and readers migrate to it in a later phase.

CREATE TABLE IF NOT EXISTS resource_shields (
  resource_id UUID        NOT NULL REFERENCES resources(id) ON DELETE CASCADE,
  shield_id   UUID        NOT NULL REFERENCES shields(id)   ON DELETE CASCADE,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  PRIMARY KEY (resource_id, shield_id)
);

CREATE INDEX IF NOT EXISTS idx_resource_shields_shield
  ON resource_shields (shield_id);

-- Backfill from the existing singular column so both agree from day one.
INSERT INTO resource_shields (resource_id, shield_id)
SELECT id, shield_id FROM resources WHERE shield_id IS NOT NULL
ON CONFLICT DO NOTHING;

-- ── Lookup support for the ACL compiler ─────────────────────────────────────

CREATE INDEX IF NOT EXISTS idx_resources_hostname
  ON resources (tenant_id, hostname)
  WHERE hostname IS NOT NULL;
