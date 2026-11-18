-- 063_relay_link_capacity.sql — Sprint 23 B2: relay link capacity for
-- bandwidth headroom (F5; Decision Record D-42, refined 2026-11-18).
--
-- An operator sets each relay's usable link throughput, in bytes per second
-- like F5's measured bandwidth (052_relay_bandwidth.sql), as the link's
-- TIGHTER direction (on an asymmetric link, usually the upload speed). NULL
-- = not configured: the console shows "Not configured" and claims no
-- headroom. A relay forwards every byte it receives, so both measured flows
-- (to clients and to connectors) share the link:
--     headroom = link_capacity_bps − (bytes_to_client_per_sec + bytes_to_connector_per_sec)
-- computed by the read API, never stored. The relay itself is unchanged: no
-- relay, proto or connector change.
ALTER TABLE relays
    ADD COLUMN link_capacity_bps BIGINT
        CHECK (link_capacity_bps IS NULL OR link_capacity_bps > 0);
