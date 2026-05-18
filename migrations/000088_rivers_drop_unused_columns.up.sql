-- Drop unused columns from rivers:
--   verified     — semantic moved to gnis_id presence (all rivers GNIS-keyed)
--   basin_locked — replaced by "basin IS NOT NULL means admin-set" semantic
-- Both columns are no longer read by any handler (2c.6c cleanup) or poller
-- (companion change in this PR drops the basin_locked clause from
-- propagateRiverBasins).
ALTER TABLE rivers
  DROP COLUMN IF EXISTS verified,
  DROP COLUMN IF EXISTS basin_locked;
