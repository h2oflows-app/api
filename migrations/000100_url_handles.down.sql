DROP INDEX IF EXISTS user_profiles_handle_lower_idx;
ALTER TABLE user_profiles DROP CONSTRAINT IF EXISTS user_profiles_handle_format;
