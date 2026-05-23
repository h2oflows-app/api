ALTER TABLE user_reaches DROP CONSTRAINT IF EXISTS user_reaches_fork_single_source;
ALTER TABLE user_reaches
  DROP COLUMN IF EXISTS forked_from_reach_id,
  DROP COLUMN IF EXISTS forked_from_user_reach_id;
