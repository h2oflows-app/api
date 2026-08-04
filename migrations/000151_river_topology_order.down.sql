ALTER TABLE rivers       DROP COLUMN IF EXISTS topology_synced_at;
ALTER TABLE gauges       DROP COLUMN IF EXISTS river_sequence;
ALTER TABLE user_reaches DROP COLUMN IF EXISTS river_sequence;
DROP INDEX IF EXISTS idx_river_flowline_order_comid;
DROP TABLE IF EXISTS river_flowline_order;
