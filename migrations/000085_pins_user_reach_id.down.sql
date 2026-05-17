DROP INDEX IF EXISTS reach_access_user_reach_id_idx;
DROP INDEX IF EXISTS reach_access_user_reach_type_name_unique;
ALTER TABLE reach_access
    DROP CONSTRAINT IF EXISTS reach_access_parent_xor,
    DROP COLUMN IF EXISTS user_reach_id,
    ALTER COLUMN reach_id SET NOT NULL;

DROP INDEX IF EXISTS rapids_user_reach_id_idx;
DROP INDEX IF EXISTS rapids_user_reach_name_unique;
ALTER TABLE rapids
    DROP CONSTRAINT IF EXISTS rapids_parent_xor,
    DROP COLUMN IF EXISTS user_reach_id,
    ALTER COLUMN reach_id SET NOT NULL;
