ALTER TABLE user_profiles
    DROP COLUMN IF EXISTS display_name,
    DROP COLUMN IF EXISTS is_special,
    DROP COLUMN IF EXISTS public_on_map,
    DROP COLUMN IF EXISTS delete_locked;
