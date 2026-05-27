ALTER TABLE user_reaches
  DROP COLUMN IF EXISTS original_author_handle,
  DROP COLUMN IF EXISTS original_author_owner_id,
  DROP COLUMN IF EXISTS original_forked_at,
  DROP COLUMN IF EXISTS last_modified_after_fork_at;
