-- Only the column comment is reversible. The pre-migration values cannot be
-- restored: they encoded "the resolver found a river", which is recoverable
-- from river_id IS NOT NULL anyway, and any true written since this migration
-- is a real human correction that would be wrong to discard.
COMMENT ON COLUMN user_reaches.river_confirmed IS NULL;
