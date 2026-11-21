-- Sprint 24 M1 (F23, Decision Record D-49): the metric store.
--
-- Postgres, no separate time-series database. The controller's sampler
-- writes one row per series and subject every minute (metric_samples, kept
-- 7 days) and rolls each finished hour up into min / max / avg / count
-- (metric_rollups, kept 400 days). An unknown value is never written: a
-- missing row is a gap, never zero.
--
-- subject_type is relay, tenant or fleet, and is the series' prefix
-- (relay.connections, tenant.active_sessions, fleet.connections). Fleet rows
-- use the nil UUID as subject_id, because primary key columns can't be NULL.
-- No foreign keys: a relay's or tenant's history outlives the row only until
-- the retention prunes it.

CREATE TABLE metric_samples (
    series       TEXT             NOT NULL,
    subject_type TEXT             NOT NULL CHECK (subject_type IN ('relay', 'tenant', 'fleet')),
    subject_id   UUID             NOT NULL,
    ts           TIMESTAMPTZ      NOT NULL,
    value        DOUBLE PRECISION NOT NULL,
    PRIMARY KEY (series, subject_id, ts),
    CONSTRAINT metric_samples_series_prefix CHECK (series LIKE subject_type || '.%'),
    CONSTRAINT metric_samples_fleet_subject CHECK (
        (subject_type = 'fleet') = (subject_id = '00000000-0000-0000-0000-000000000000')),
    -- On the minute, independent of the session time zone.
    CONSTRAINT metric_samples_on_minute CHECK (EXTRACT(EPOCH FROM ts)::bigint % 60 = 0
                                               AND EXTRACT(EPOCH FROM ts) = EXTRACT(EPOCH FROM ts)::bigint),
    -- Postgres treats NaN as equal to NaN, so compare with it explicitly.
    CONSTRAINT metric_samples_finite CHECK (value <> 'NaN' AND value <> 'Infinity' AND value <> '-Infinity')
);

-- Retention deletes by time.
CREATE INDEX metric_samples_ts ON metric_samples (ts);

CREATE TABLE metric_rollups (
    series       TEXT             NOT NULL,
    subject_type TEXT             NOT NULL CHECK (subject_type IN ('relay', 'tenant', 'fleet')),
    subject_id   UUID             NOT NULL,
    ts           TIMESTAMPTZ      NOT NULL, -- start of the hour
    min          DOUBLE PRECISION NOT NULL,
    max          DOUBLE PRECISION NOT NULL,
    avg          DOUBLE PRECISION NOT NULL,
    count        INTEGER          NOT NULL CHECK (count BETWEEN 1 AND 60), -- minutes with a sample
    PRIMARY KEY (series, subject_id, ts),
    CONSTRAINT metric_rollups_series_prefix CHECK (series LIKE subject_type || '.%'),
    CONSTRAINT metric_rollups_fleet_subject CHECK (
        (subject_type = 'fleet') = (subject_id = '00000000-0000-0000-0000-000000000000')),
    -- On the hour, independent of the session time zone (date_trunc('hour')
    -- would follow it, and some zones are offset by 30 or 45 minutes).
    CONSTRAINT metric_rollups_on_hour CHECK (EXTRACT(EPOCH FROM ts)::bigint % 3600 = 0
                                             AND EXTRACT(EPOCH FROM ts) = EXTRACT(EPOCH FROM ts)::bigint),
    -- The roll-up clamps avg into [min, max] (a float mean can round past them).
    CONSTRAINT metric_rollups_ordered CHECK (min <= avg AND avg <= max)
);

CREATE INDEX metric_rollups_ts ON metric_rollups (ts);

-- The app writes samples and deletes expired ones; a roll-up is upserted, so
-- re-running an hour (after a restart) replaces it.
GRANT SELECT, INSERT, DELETE ON metric_samples TO ztna_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON metric_rollups TO ztna_app;
