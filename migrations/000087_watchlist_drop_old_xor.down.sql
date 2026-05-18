ALTER TABLE user_watchlists
  ADD CONSTRAINT user_watchlists_one_target_chk
    CHECK ((gauge_id IS NOT NULL) <> (custom_gauge_id IS NOT NULL));
