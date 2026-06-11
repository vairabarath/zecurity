-- 018_shield_snapshot_generation.sql — ADR-004 Phase 2 / Code-study Finding F11
--
-- Replace the wall-clock-millis snapshot generation with a per-shield monotonic
-- counter. The old scheme (time.Now().UnixMilli()) is non-monotonic across an NTP
-- step backwards: a newer snapshot could carry a LOWER generation, so the shield's
-- `generation <= last` staleness gate would silently drop it and keep stale rules.
--
-- These two columns are OPAQUE bookkeeping — they carry no desired-state semantics.
-- The definition of "what a shield should enforce" lives in exactly one place,
-- resource.desiredForShield (Go). buildSnapshotMsg hashes the rows that function
-- returns into snapshot_fingerprint and bumps snapshot_generation only when that
-- fingerprint changes, all inside one transaction with a row lock on the shield.
-- So the generation tracks real content changes (no churn from metadata/audit
-- writes), is consistent with the content it stamps, and survives controller
-- restarts. Nothing in SQL knows the desired-state rule, so it cannot drift from Go.

ALTER TABLE shields
    ADD COLUMN snapshot_generation  BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN snapshot_fingerprint TEXT   NOT NULL DEFAULT '';
