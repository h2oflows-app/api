ALTER TABLE user_reaches ADD COLUMN long_name TEXT;

-- Backfill forked runs: long_name = full curated reach name.
-- Gives forked runs a descriptive name alongside their inherited short common_name.
UPDATE user_reaches ur
SET long_name = r.name
FROM reaches r
WHERE ur.forked_from_reach_id = r.id
  AND ur.long_name IS NULL;
