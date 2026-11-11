-- F5: relay bandwidth. The relay reports cumulative bytes forwarded on bridged
-- client streams per direction since its process started (heartbeat fields 9
-- and 10). RecordHeartbeat keeps the latest sample and derives the average
-- rate since the previous stored sample. Heartbeats reach the database every
-- RELAY_HEARTBEAT_DB_WRITE_INTERVAL (default 5 min; every heartbeat while
-- draining), so the rate is an average over that window, stored with it.
--
-- NULL totals: the relay doesn't report bytes (older relay). NULL rates with a
-- sample: the first sample, or the relay restarted between samples (a counter
-- went down, or its uptime is shorter than the gap); the next sample has one.
ALTER TABLE relays
    ADD COLUMN bytes_to_connector_total   BIGINT CHECK (bytes_to_connector_total >= 0),
    ADD COLUMN bytes_to_client_total      BIGINT CHECK (bytes_to_client_total >= 0),
    ADD COLUMN bytes_sampled_at           TIMESTAMPTZ,
    ADD COLUMN bytes_to_connector_per_sec DOUBLE PRECISION CHECK (bytes_to_connector_per_sec >= 0),
    ADD COLUMN bytes_to_client_per_sec    DOUBLE PRECISION CHECK (bytes_to_client_per_sec >= 0),
    ADD COLUMN bytes_rate_window_seconds  DOUBLE PRECISION CHECK (bytes_rate_window_seconds > 0);
