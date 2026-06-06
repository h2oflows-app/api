-- Backfill gauge_id on user_watchlists entries that have reach_slug but no gauge_id.
-- Two cases:
--   1. A gauged row (same user/gauge/reach/dashboard) already exists → DELETE the null duplicate.
--   2. No gauged duplicate → UPDATE to set gauge_id from user_reaches.primary_gauge_id.

-- Step 1: delete null-gauge rows where the gauged version already exists
DELETE FROM user_watchlists uw
USING user_reaches ur
WHERE uw.reach_slug = ur.slug
  AND uw.gauge_id IS NULL
  AND uw.reach_slug IS NOT NULL
  AND ur.primary_gauge_id IS NOT NULL
  AND EXISTS (
    SELECT 1 FROM user_watchlists uw2
    WHERE uw2.user_id      = uw.user_id
      AND uw2.gauge_id     = ur.primary_gauge_id
      AND uw2.reach_slug   = uw.reach_slug
      AND COALESCE(uw2.dashboard_id::text, '') = COALESCE(uw.dashboard_id::text, '')
      AND uw2.id != uw.id
  );

-- Step 2: update remaining null-gauge rows that have no gauged duplicate
UPDATE user_watchlists uw
SET gauge_id = ur.primary_gauge_id
FROM user_reaches ur
WHERE uw.reach_slug = ur.slug
  AND uw.gauge_id IS NULL
  AND uw.reach_slug IS NOT NULL
  AND ur.primary_gauge_id IS NOT NULL;
