-- Backfill gauge_id on user_watchlists entries that have reach_slug but no gauge_id.
-- Rows added via addReachToWatchlist (no gauge context) won't appear as WatchedGauge
-- items in the frontend, so groupByGauge has no effect on them.
-- Sets gauge_id from user_reaches.primary_gauge_id where available.

UPDATE user_watchlists uw
SET gauge_id = ur.primary_gauge_id
FROM user_reaches ur
WHERE uw.reach_slug = ur.slug
  AND uw.gauge_id IS NULL
  AND uw.reach_slug IS NOT NULL
  AND ur.primary_gauge_id IS NOT NULL;
