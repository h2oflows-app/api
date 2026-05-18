DROP INDEX IF EXISTS user_watchlists_user_reach_only_uk;

ALTER TABLE user_watchlists
  DROP CONSTRAINT IF EXISTS user_watchlists_gauge_xor,
  ADD CONSTRAINT user_watchlists_gauge_xor
    CHECK ((gauge_id IS NOT NULL) <> (custom_gauge_id IS NOT NULL));
