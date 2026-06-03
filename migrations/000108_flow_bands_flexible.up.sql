-- Umbrella J: flexible per-run flow bands.
-- Old model: 3 fixed rows (low/running/high) w/ min_value/max_value.
-- New model: base (label+color on parent table) + 0..N threshold rows (value+label+color).
-- Band logic: cfs >= highest threshold value → that band; else → base.

BEGIN;

-- 1. Add base columns to parent tables.
ALTER TABLE reaches
  ADD COLUMN base_label TEXT NOT NULL DEFAULT 'Too Low',
  ADD COLUMN base_color TEXT NOT NULL DEFAULT 'red-3';

ALTER TABLE user_reaches
  ADD COLUMN base_label TEXT NOT NULL DEFAULT 'Too Low',
  ADD COLUMN base_color TEXT NOT NULL DEFAULT 'red-3';

-- 2. Add new columns to range tables (value nullable until backfill).
ALTER TABLE flow_ranges
  ADD COLUMN value NUMERIC,
  ADD COLUMN color TEXT NOT NULL DEFAULT 'green-3';

ALTER TABLE user_reach_flow_ranges
  ADD COLUMN value NUMERIC,
  ADD COLUMN color TEXT NOT NULL DEFAULT 'green-3';

-- 3. Drop old label constraints (labels are now arbitrary user text).
ALTER TABLE flow_ranges
  DROP CONSTRAINT flow_ranges_label_check,
  DROP CONSTRAINT flow_ranges_reach_id_label_key;

ALTER TABLE user_reach_flow_ranges
  DROP CONSTRAINT user_reach_flow_ranges_label_check,
  DROP CONSTRAINT user_reach_flow_ranges_user_reach_id_label_key;

-- 4. Backfill flow_ranges.
--    running row → 'Running' threshold (value=min_value).
--    insert new 'High' threshold (value=running.max_value).
--    delete old 'low' and 'high' rows (boundary mirrors — now implicit).

CREATE TEMP TABLE _fr_running AS
  SELECT id, reach_id, gauge_id, data_source, verified, min_value, max_value
  FROM flow_ranges
  WHERE label = 'running';

UPDATE flow_ranges
  SET value = min_value, label = 'Running', color = 'green-3'
  WHERE label = 'running';

INSERT INTO flow_ranges (reach_id, gauge_id, label, value, color, data_source, verified)
  SELECT reach_id, gauge_id, 'High', max_value, 'blue-3', data_source, verified
  FROM _fr_running
  WHERE max_value IS NOT NULL;

DELETE FROM flow_ranges WHERE label IN ('low', 'high');

DROP TABLE _fr_running;

-- 5. Backfill user_reach_flow_ranges (same pattern, simpler columns).

CREATE TEMP TABLE _urfr_running AS
  SELECT id, user_reach_id, min_value, max_value
  FROM user_reach_flow_ranges
  WHERE label = 'running';

UPDATE user_reach_flow_ranges
  SET value = min_value, label = 'Running', color = 'green-3'
  WHERE label = 'running';

INSERT INTO user_reach_flow_ranges (user_reach_id, label, value, color)
  SELECT user_reach_id, 'High', max_value, 'blue-3'
  FROM _urfr_running
  WHERE max_value IS NOT NULL;

DELETE FROM user_reach_flow_ranges WHERE label IN ('low', 'high');

DROP TABLE _urfr_running;

-- 6. Set value NOT NULL now that backfill is done.
ALTER TABLE flow_ranges ALTER COLUMN value SET NOT NULL;
ALTER TABLE user_reach_flow_ranges ALTER COLUMN value SET NOT NULL;

-- 7. Drop obsolete columns.
ALTER TABLE flow_ranges
  DROP COLUMN min_value,
  DROP COLUMN max_value,
  DROP COLUMN IF EXISTS legacy_band_data;

ALTER TABLE user_reach_flow_ranges
  DROP COLUMN min_value,
  DROP COLUMN max_value;

-- 8. Add UNIQUE(reach_id, value) to prevent duplicate thresholds at same CFS.
ALTER TABLE flow_ranges
  ADD CONSTRAINT flow_ranges_reach_id_value_key UNIQUE (reach_id, value);

ALTER TABLE user_reach_flow_ranges
  ADD CONSTRAINT user_reach_flow_ranges_user_reach_id_value_key UNIQUE (user_reach_id, value);

COMMIT;
