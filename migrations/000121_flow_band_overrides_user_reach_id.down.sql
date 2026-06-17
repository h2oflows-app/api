DROP INDEX IF EXISTS reach_flow_band_overrides_user_reach_idx;
ALTER TABLE reach_flow_band_overrides
  DROP CONSTRAINT IF EXISTS reach_flow_band_overrides_user_reach_user_id_key,
  DROP COLUMN IF EXISTS user_reach_id;
