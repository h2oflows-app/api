-- Fix duplicate watchlist entries for reach-only and reference rows.
-- Migration 082 added unique constraints for gauge_id / custom_gauge_id rows
-- but left reach-only (gauge_id IS NULL, custom_gauge_id IS NULL, referenced_user_reach_id IS NULL)
-- and reference (referenced_user_reach_id IS NOT NULL) rows unconstrained.
-- ON CONFLICT DO NOTHING on those inserts was a no-op — every re-add created a new row.

-- 1. Dedup reach-only rows — keep oldest per (user_id, reach_slug, dashboard_id).
DELETE FROM user_watchlists
WHERE id IN (
  SELECT id FROM (
    SELECT id,
           ROW_NUMBER() OVER (
             PARTITION BY user_id, COALESCE(reach_slug, ''), dashboard_id
             ORDER BY created_at
           ) AS rn
    FROM user_watchlists
    WHERE gauge_id IS NULL
      AND custom_gauge_id IS NULL
      AND referenced_user_reach_id IS NULL
  ) ranked
  WHERE rn > 1
);

-- 2. Dedup reference rows — keep oldest per (user_id, referenced_user_reach_id, dashboard_id).
DELETE FROM user_watchlists
WHERE id IN (
  SELECT id FROM (
    SELECT id,
           ROW_NUMBER() OVER (
             PARTITION BY user_id, referenced_user_reach_id, dashboard_id
             ORDER BY created_at
           ) AS rn
    FROM user_watchlists
    WHERE referenced_user_reach_id IS NOT NULL
  ) ranked
  WHERE rn > 1
);

-- 3. Backfill gauge_id on reach-only rows where the user reach has a primary_gauge_id.
--    Converts existing (user_id, reach_slug, dashboard_id) rows to
--    (user_id, gauge_id, reach_slug, dashboard_id) so group-by-gauge works and the
--    migration-082 unique constraint prevents future duplicates.
--    Skip any that would conflict with an already-existing gauged row for the same run.
UPDATE user_watchlists w
SET    gauge_id = ur.primary_gauge_id
FROM   user_reaches ur
WHERE  ur.slug       = w.reach_slug
  AND  ur.deleted_at IS NULL
  AND  ur.primary_gauge_id IS NOT NULL
  AND  w.gauge_id          IS NULL
  AND  w.custom_gauge_id   IS NULL
  AND  w.referenced_user_reach_id IS NULL
  AND  NOT EXISTS (
         SELECT 1 FROM user_watchlists x
         WHERE  x.user_id      = w.user_id
           AND  x.gauge_id     = ur.primary_gauge_id
           AND  COALESCE(x.reach_slug, '') = COALESCE(w.reach_slug, '')
           AND  x.dashboard_id = w.dashboard_id
           AND  x.id          <> w.id
       );

-- 4. Add unique constraints.
CREATE UNIQUE INDEX IF NOT EXISTS user_watchlists_reach_only_uk
  ON user_watchlists (user_id, COALESCE(reach_slug, ''), dashboard_id)
  WHERE gauge_id IS NULL
    AND custom_gauge_id IS NULL
    AND referenced_user_reach_id IS NULL;

CREATE UNIQUE INDEX IF NOT EXISTS user_watchlists_reference_uk
  ON user_watchlists (user_id, referenced_user_reach_id, dashboard_id)
  WHERE referenced_user_reach_id IS NOT NULL;
