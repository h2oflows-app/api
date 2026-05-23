DROP INDEX IF EXISTS user_reaches_public_idx;
ALTER TABLE user_reaches
  DROP COLUMN IF EXISTS is_private;
