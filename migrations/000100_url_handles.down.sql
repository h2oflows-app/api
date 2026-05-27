-- Revert constraint + index only. Does NOT restore pre-rename handles (V15).
ALTER TABLE user_profiles DROP CONSTRAINT IF EXISTS user_profiles_handle_format;
DROP INDEX IF EXISTS user_profiles_handle_lower_idx;
ALTER TABLE user_profiles ADD CONSTRAINT user_profiles_handle_key UNIQUE (handle);
