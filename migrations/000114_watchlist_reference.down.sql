DROP INDEX IF EXISTS user_watchlists_reference_idx;

ALTER TABLE user_watchlists
  DROP COLUMN referenced_user_reach_id;
