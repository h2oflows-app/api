ALTER TABLE user_watchlists
  DROP CONSTRAINT user_watchlists_gauge_xor;

ALTER TABLE user_watchlists
  ADD CONSTRAINT user_watchlists_gauge_xor
    CHECK (
      (gauge_id IS NULL OR custom_gauge_id IS NULL)
      AND (gauge_id IS NOT NULL OR custom_gauge_id IS NOT NULL OR reach_slug IS NOT NULL)
    );
