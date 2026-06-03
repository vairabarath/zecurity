ALTER TABLE resources DROP CONSTRAINT resources_status_check;
ALTER TABLE resources ADD CONSTRAINT resources_status_check
    CHECK (status IN ('pending','protecting','protected','unprotected','failed','deleting','deleted'));

-- Replace the stale partial index from migration 007: its predicate referenced
-- the retired 'managing'/'removing' states, so it matched zero rows after 009.
-- Cover the states the reconnect/delivery path actually queries.
DROP INDEX IF EXISTS idx_resources_managing;
CREATE INDEX idx_resources_pending
    ON resources (shield_id, status)
    WHERE status IN ('protecting','deleting') AND deleted_at IS NULL;
