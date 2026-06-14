-- Reverse Phase 1 dual-parent on hazards and reach_conditions.
-- Any rows parented on user_reach_id would violate the restored NOT NULL;
-- delete them first (they only exist if user-authored hazards/conditions were
-- created after this migration).

DELETE FROM hazards          WHERE user_reach_id IS NOT NULL;
DELETE FROM reach_conditions WHERE user_reach_id IS NOT NULL;

DROP INDEX IF EXISTS reach_conditions_user_reach_expiry_idx;
DROP INDEX IF EXISTS reach_conditions_user_reach_id_idx;
ALTER TABLE reach_conditions DROP CONSTRAINT IF EXISTS reach_conditions_parent_xor;
ALTER TABLE reach_conditions DROP COLUMN IF EXISTS user_reach_id;
ALTER TABLE reach_conditions ALTER COLUMN reach_id SET NOT NULL;

DROP INDEX IF EXISTS hazards_user_reach_active_idx;
DROP INDEX IF EXISTS hazards_user_reach_id_idx;
ALTER TABLE hazards DROP CONSTRAINT IF EXISTS hazards_parent_xor;
ALTER TABLE hazards DROP COLUMN IF EXISTS user_reach_id;
ALTER TABLE hazards ALTER COLUMN reach_id SET NOT NULL;
