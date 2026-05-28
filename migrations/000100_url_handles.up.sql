-- handle regex constraint + unique lower(handle) index
ALTER TABLE user_profiles
  DROP CONSTRAINT IF EXISTS user_profiles_handle_format;
ALTER TABLE user_profiles
  ADD CONSTRAINT user_profiles_handle_format
  CHECK (handle ~ '^[a-zA-Z0-9_\-]{2,30}$');

CREATE UNIQUE INDEX IF NOT EXISTS user_profiles_handle_lower_idx
  ON user_profiles (lower(handle));
