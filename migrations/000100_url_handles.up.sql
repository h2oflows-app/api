-- Bulk-rename handles to URL-safe format (V14):
--   spaces → "-", strip [^a-zA-Z0-9_-], truncate to 30, suffix -2/-3 on collision.
-- Idempotent: rows already clean are unchanged.
WITH cleaned AS (
  SELECT owner_id,
    LEFT(
      REGEXP_REPLACE(
        REGEXP_REPLACE(handle, '\s+', '-', 'g'),
        '[^a-zA-Z0-9_\-]', '', 'g'
      ), 30
    ) AS base_clean
  FROM user_profiles
),
padded AS (
  SELECT owner_id,
    CASE
      WHEN LENGTH(base_clean) = 0 THEN 'user'
      WHEN LENGTH(base_clean) = 1 THEN base_clean || '1'
      ELSE base_clean
    END AS base_clean
  FROM cleaned
),
ranked AS (
  SELECT owner_id, base_clean,
    ROW_NUMBER() OVER (PARTITION BY LOWER(base_clean) ORDER BY owner_id) AS rn
  FROM padded
),
final AS (
  SELECT owner_id,
    CASE
      WHEN rn = 1 THEN LEFT(base_clean, 30)
      ELSE LEFT(base_clean, 30 - LENGTH('-' || rn::text)) || '-' || rn::text
    END AS new_handle
  FROM ranked
)
UPDATE user_profiles p
SET handle = f.new_handle
FROM final f
WHERE f.owner_id = p.owner_id
  AND f.new_handle IS DISTINCT FROM p.handle;

-- Drop case-sensitive uniqueness; replace with case-insensitive + format check (V1).
ALTER TABLE user_profiles DROP CONSTRAINT user_profiles_handle_key;
CREATE UNIQUE INDEX user_profiles_handle_lower_idx ON user_profiles (LOWER(handle));
ALTER TABLE user_profiles ADD CONSTRAINT user_profiles_handle_format
  CHECK (handle ~ '^[a-zA-Z0-9_\-]{2,30}$');
