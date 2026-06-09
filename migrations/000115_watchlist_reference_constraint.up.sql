-- Fix gauge_xor constraint to allow reference-only rows (referenced_user_reach_id set).
-- Previous constraint required gauge_id|custom_gauge_id|reach_slug; reference rows have none.

ALTER TABLE user_watchlists
  DROP CONSTRAINT user_watchlists_gauge_xor;

ALTER TABLE user_watchlists
  ADD CONSTRAINT user_watchlists_gauge_xor
    CHECK (
      (gauge_id IS NULL OR custom_gauge_id IS NULL)
      AND (
        gauge_id IS NOT NULL OR
        custom_gauge_id IS NOT NULL OR
        reach_slug IS NOT NULL OR
        referenced_user_reach_id IS NOT NULL
      )
    );
