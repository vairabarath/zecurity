-- 017_resources_drop_soft_delete.sql
--
-- ADR-004 Phase 4.2 (Finding 8): drop the vestigial soft-delete scaffolding on
-- the resources table.
--
-- History: resources were originally designed for soft-delete (mig 007 added a
-- `deleted_at` column, a `'deleted'` status, and `deleted_at IS NULL` partial
-- indexes). The implementation instead hard-deletes (DELETE FROM), and ADR-004
-- made the tombstone the `deleting` status + ack-gated reap — also a hard DELETE.
-- So `deleted_at` is never written (always NULL), the `'deleted'` status is
-- unreachable, and every `deleted_at IS NULL` filter is a no-op. Remove the dead
-- scaffolding so the schema states the real model.
--
-- NOTE: this targets ONLY the resources table. The `'deleted'` status on
-- workspaces / users / connectors / shields / remote_networks is a real, in-use
-- soft-delete and is left untouched.

-- A column cannot be dropped while an index depends on it — drop the indexes,
-- drop the column, then recreate the indexes without the dead predicate.
DROP INDEX IF EXISTS idx_resources_shield;
DROP INDEX IF EXISTS idx_resources_pending;

ALTER TABLE resources DROP COLUMN IF EXISTS deleted_at;

-- Recreate without the `WHERE deleted_at IS NULL` predicate.
CREATE INDEX idx_resources_shield
    ON resources (shield_id);

-- Mirrors mig 015's predicate minus the dead `deleted_at IS NULL` clause.
CREATE INDEX idx_resources_pending
    ON resources (shield_id, status)
    WHERE status IN ('protecting', 'deleting');

-- Remove the unreachable 'deleted' value from the status enum. Safe: no resource
-- row can hold it (nothing ever assigned it). 'deleting' is the real tombstone.
ALTER TABLE resources DROP CONSTRAINT resources_status_check;
ALTER TABLE resources ADD CONSTRAINT resources_status_check
    CHECK (status IN ('pending','protecting','protected','unprotected','failed','deleting'));
