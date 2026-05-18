-- Allow watchlist entries with just reach_slug (no gauge) so ungauged user
-- reaches can be pinned to a dashboard. Also tightens the constraint to
-- prevent completely empty rows.

ALTER TABLE user_watchlists
  DROP CONSTRAINT IF EXISTS user_watchlists_gauge_xor,
  ADD CONSTRAINT user_watchlists_gauge_xor
    CHECK (
      -- Exactly one gauge type if any gauge is set, AND at least one field non-null
      (gauge_id IS NULL OR custom_gauge_id IS NULL)
      AND (gauge_id IS NOT NULL OR custom_gauge_id IS NOT NULL OR reach_slug IS NOT NULL)
    );

-- Partial unique index so the same ungauged reach can't be pinned twice on a dashboard.
CREATE UNIQUE INDEX user_watchlists_user_reach_only_uk
  ON user_watchlists (user_id, reach_slug, dashboard_id)
  WHERE gauge_id IS NULL AND custom_gauge_id IS NULL AND reach_slug IS NOT NULL;
