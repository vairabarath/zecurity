-- Drop the source column from connector_relay_placement.
--
-- 022 originally declared a 'source' column (CHECK ('event' | 'heartbeat'))
-- as diagnostic provenance for each row. It turned out unused in any code
-- path or query, so we're dropping it.
--
-- IF EXISTS so this migration is a no-op on databases that started from a
-- 022 file that already excludes the column (fresh installs from the
-- post-edit checkout).

ALTER TABLE connector_relay_placement DROP COLUMN IF EXISTS source;
