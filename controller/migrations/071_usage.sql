-- Sprint 25 U1a (F32, Decision Record D-57, D-58, D-59): the usage ledger.
--
-- Usage is counted, never estimated (D-59): exact relayed bytes from R1's
-- cumulative per-trust-domain counters (deltas between heartbeats of one
-- relay run), and session / connector / shield minutes and monthly active
-- users counted once per collected minute. It is a separate ledger, never
-- derived from the M1 metric store (068_, rates and averages for charts).
--
-- All times are UTC buckets (D-57). Quantities are integers. Usage outlives
-- a purged tenant (D-58, amends D-46), so there are no foreign keys to
-- workspaces, relays or users. Everything is kept 25 months, then pruned by
-- the collector.

-- Meters per tenant and UTC hour. A row is added to (upsert) by the
-- collector; a quantity never goes down.
CREATE TABLE usage_hourly (
    tenant_id UUID        NOT NULL,
    meter     TEXT        NOT NULL CHECK (meter IN ('relay_bytes_to_clients', 'relay_bytes_to_connectors',
                                                    'session_minutes', 'connector_minutes', 'shield_minutes')),
    hour      TIMESTAMPTZ NOT NULL,
    quantity  BIGINT      NOT NULL CHECK (quantity >= 0),
    PRIMARY KEY (tenant_id, meter, hour),
    -- On the hour, independent of the session time zone (as metric_rollups).
    CONSTRAINT usage_hourly_on_hour CHECK (EXTRACT(EPOCH FROM hour)::bigint % 3600 = 0
                                           AND EXTRACT(EPOCH FROM hour) = EXTRACT(EPOCH FROM hour)::bigint)
);

-- Retention deletes by time; the fleet list reads one month for every tenant.
CREATE INDEX usage_hourly_hour ON usage_hourly (hour);

-- Completeness (D-59): one row per minute the collector counted, fleet-wide
-- (the same for every tenant). An hour is complete when all 60 of its minutes
-- are here; a minute the controller missed has no row and is never back-filled.
-- The row is written in the same transaction as that minute's counts, so a
-- re-run of the minute finds it and counts nothing twice.
CREATE TABLE usage_collector_minutes (
    minute TIMESTAMPTZ PRIMARY KEY,
    CONSTRAINT usage_collector_minutes_on_minute CHECK (EXTRACT(EPOCH FROM minute)::bigint % 60 = 0
                                                        AND EXTRACT(EPOCH FROM minute) = EXTRACT(EPOCH FROM minute)::bigint)
);

-- Monthly active users (D-57): distinct users with a live client session at
-- any collected minute of the UTC month. One row per user per month; the
-- monthly figure is count(*). A distinct count can't be added up from hours,
-- hence its own table.
CREATE TABLE usage_active_users (
    tenant_id  UUID        NOT NULL,
    month      DATE        NOT NULL CHECK (EXTRACT(DAY FROM month) = 1),
    user_id    UUID        NOT NULL,
    first_seen TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, month, user_id)
);

CREATE INDEX usage_active_users_month ON usage_active_users (month);

-- A purged tenant's monthly active users as counts only (D-58): the purge
-- moves its usage_active_users rows here as one number per month, so no user
-- ids of a deleted tenant are kept.
CREATE TABLE usage_active_user_counts (
    tenant_id UUID    NOT NULL,
    month     DATE    NOT NULL CHECK (EXTRACT(DAY FROM month) = 1),
    users     INTEGER NOT NULL CHECK (users >= 0),
    PRIMARY KEY (tenant_id, month)
);

-- The tenant as reports name it (D-58): kept current by the collector while
-- the tenant exists and frozen at purge, so usage of a deleted tenant still
-- carries its id and name until retention ends.
CREATE TABLE usage_tenants (
    tenant_id  UUID        PRIMARY KEY,
    name       TEXT        NOT NULL,
    slug       TEXT        NOT NULL,
    plan_name  TEXT,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    purged_at  TIMESTAMPTZ
);

-- The relay counters already counted, per relay and tenant (D-59). Written in
-- the same transaction as the usage they produced, so a controller restart
-- (the in-memory R1 tracker starts empty) resumes from here: no byte lost,
-- none counted twice. relay_started_at (heartbeat time minus uptime) tells a
-- relay restart apart: a new run starts a new baseline.
CREATE TABLE usage_relay_baselines (
    relay_id         UUID        NOT NULL,
    tenant_id        UUID        NOT NULL,
    relay_started_at TIMESTAMPTZ NOT NULL,
    to_clients       BIGINT      NOT NULL CHECK (to_clients >= 0),
    to_connectors    BIGINT      NOT NULL CHECK (to_connectors >= 0),
    at               TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (relay_id, tenant_id)
);

-- Privileges: 053's ALTER DEFAULT PRIVILEGES gives ztna_app SELECT, INSERT,
-- UPDATE and DELETE on these tables (the collector adds usage by upsert,
-- writes baselines, prunes past retention; the purge rewrites a tenant's
-- active users as counts). A collected minute is only ever inserted or
-- pruned, never rewritten.
REVOKE UPDATE ON usage_collector_minutes FROM ztna_app;
