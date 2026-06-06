-- Revert: clear gauge_id from watchlist entries whose reach_slug links to a
-- user_reach with that primary_gauge_id (i.e. the rows we backfilled).
-- Cannot restore rows that already had a gauge_id before the migration.

UPDATE user_watchlists uw
SET gauge_id = NULL
FROM user_reaches ur
WHERE uw.reach_slug = ur.slug
  AND uw.gauge_id = ur.primary_gauge_id
  AND uw.reach_slug IS NOT NULL;
