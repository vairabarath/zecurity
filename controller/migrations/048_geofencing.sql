-- 048_geofencing.sql — country geofencing as a posture check (PENDING-20 G1).
--
-- 1. device_profile_requirements.params: per-requirement settings, validated
--    per check by the controller. Only zecurity.network.country uses it:
--    {"allow": ["IN", ...], "deny": ["XX", ...]} (ISO 3166-1 alpha-2).
-- 2. device_network_observations: the country the controller last saw a
--    client device connect from (its gRPC peer address looked up in a local
--    GeoIP database). Only the country and the address's /24 (IPv4) or /48
--    (IPv6) prefix are stored, never the full address (G-6).
--
-- Pre-production rule: applied only when the Postgres volume is created.
-- [schema: reset DB]

ALTER TABLE device_profile_requirements
    ADD COLUMN params JSONB NOT NULL DEFAULT '{}'::jsonb
        CHECK (jsonb_typeof(params) = 'object');

CREATE TABLE device_network_observations (
    device_id      UUID        NOT NULL PRIMARY KEY,
    workspace_id   UUID        NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,

    -- ISO 3166-1 alpha-2, or '' when the address has no country (private,
    -- reserved, not in the database, lookup failed, database not loaded).
    country        TEXT        NOT NULL DEFAULT ''
                               CHECK (country = '' OR country ~ '^[A-Z]{2}$'),

    -- /24 (IPv4) or /48 (IPv6) of the observed address, for troubleshooting.
    network_prefix CIDR,

    -- When the device was last seen (any client RPC that records it).
    observed_at    TIMESTAMPTZ NOT NULL,
    -- When `country` last changed; observed_at moves on every sighting.
    country_since  TIMESTAMPTZ NOT NULL,
    -- Build date of the GeoIP database that produced `country`.
    geoip_build    TIMESTAMPTZ,

    CONSTRAINT device_network_observations_device_same_workspace_fkey
        FOREIGN KEY (device_id, workspace_id)
        REFERENCES client_devices (id, workspace_id) ON DELETE CASCADE
);
