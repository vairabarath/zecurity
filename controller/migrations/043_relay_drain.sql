-- Sprint 22 Phase D: relay drain and resume (Decision Record D-32).
--
-- drain_started_at: set while a relay is draining. A draining relay is left out
-- of the relay list published to connectors (internal/relay/eligible), so no new
-- placements reach it and connectors migrate away through their existing path.
-- A column, not a status: heartbeats move status between active and inactive,
-- and drain is independent of that.
--
-- connection_count_at: when the stored connection_count was measured (the
-- controller's receive time of the heartbeat that carried it). The count is
-- persisted at most every RELAY_HEARTBEAT_DB_WRITE_INTERVAL outside a drain, so
-- the provider console needs its age before it may call a relay drained.
--
-- [schema: reset DB] — no migration framework; recreate local databases.

ALTER TABLE relays ADD COLUMN drain_started_at    TIMESTAMPTZ;
ALTER TABLE relays ADD COLUMN connection_count_at TIMESTAMPTZ;
