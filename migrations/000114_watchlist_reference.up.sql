-- Add referenced_user_reach_id to user_watchlists for reference (no-copy) adds.
-- NULL = legacy own-run or gauge entry. Non-null = pointer at canonical run. (V6)

ALTER TABLE user_watchlists
  ADD COLUMN referenced_user_reach_id UUID REFERENCES user_reaches(id) ON DELETE SET NULL;

CREATE INDEX user_watchlists_reference_idx
  ON user_watchlists (user_id, referenced_user_reach_id)
  WHERE referenced_user_reach_id IS NOT NULL;
