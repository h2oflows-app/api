-- Reverse migration 108. Best-effort reconstruction of 3-band model from
-- flexible threshold rows. Assumes only 'Running' and 'High' threshold labels.

BEGIN;

-- 1. Add back old columns (nullable for reconstruction).
ALTER TABLE flow_ranges
  ADD COLUMN min_value NUMERIC,
  ADD COLUMN max_value NUMERIC,
  ADD COLUMN legacy_band_data JSONB;

ALTER TABLE user_reach_flow_ranges
  ADD COLUMN min_value NUMERIC,
  ADD COLUMN max_value NUMERIC;

-- 2. Drop UNIQUE(reach_id, value) constraints added in up migration.
ALTER TABLE flow_ranges
  DROP CONSTRAINT flow_ranges_reach_id_value_key;

ALTER TABLE user_reach_flow_ranges
  DROP CONSTRAINT user_reach_flow_ranges_user_reach_id_value_key;

-- 3. Reconstruct flow_ranges 3-band rows from 'Running' + 'High' thresholds.

CREATE TEMP TABLE _fr_high AS
  SELECT reach_id, value AS high_val
  FROM flow_ranges
  WHERE label = 'High';

-- Convert 'Running' threshold → 'running' row.
UPDATE flow_ranges fr
  SET min_value = fr.value,
      max_value = h.high_val,
      label     = 'running'
  FROM _fr_high h
  WHERE fr.reach_id = h.reach_id AND fr.label = 'Running';

-- Insert 'low' row (max_value = running.min_value).
INSERT INTO flow_ranges (reach_id, gauge_id, label, min_value, max_value, data_source, verified)
  SELECT reach_id, gauge_id, 'low', NULL, min_value, data_source, verified
  FROM flow_ranges
  WHERE label = 'running';

-- Insert 'high' row (min_value = running.max_value).
INSERT INTO flow_ranges (reach_id, gauge_id, label, min_value, max_value, data_source, verified)
  SELECT reach_id, gauge_id, 'high', max_value, NULL, data_source, verified
  FROM flow_ranges
  WHERE label = 'running' AND max_value IS NOT NULL;

-- Delete 'High' threshold rows.
DELETE FROM flow_ranges WHERE label = 'High';

DROP TABLE _fr_high;

-- 4. Reconstruct user_reach_flow_ranges 3-band rows.

CREATE TEMP TABLE _urfr_high AS
  SELECT user_reach_id, value AS high_val
  FROM user_reach_flow_ranges
  WHERE label = 'High';

UPDATE user_reach_flow_ranges ur
  SET min_value = ur.value,
      max_value = h.high_val,
      label     = 'running'
  FROM _urfr_high h
  WHERE ur.user_reach_id = h.user_reach_id AND ur.label = 'Running';

INSERT INTO user_reach_flow_ranges (user_reach_id, label, min_value, max_value)
  SELECT user_reach_id, 'low', NULL, min_value
  FROM user_reach_flow_ranges
  WHERE label = 'running';

INSERT INTO user_reach_flow_ranges (user_reach_id, label, min_value, max_value)
  SELECT user_reach_id, 'high', max_value, NULL
  FROM user_reach_flow_ranges
  WHERE label = 'running' AND max_value IS NOT NULL;

DELETE FROM user_reach_flow_ranges WHERE label = 'High';

DROP TABLE _urfr_high;

-- 5. Drop new columns.
ALTER TABLE flow_ranges
  DROP COLUMN value,
  DROP COLUMN color;

ALTER TABLE user_reach_flow_ranges
  DROP COLUMN value,
  DROP COLUMN color;

-- 6. Drop base columns from parent tables.
ALTER TABLE reaches
  DROP COLUMN base_label,
  DROP COLUMN base_color;

ALTER TABLE user_reaches
  DROP COLUMN base_label,
  DROP COLUMN base_color;

-- 7. Restore old label CHECK and UNIQUE constraints.
ALTER TABLE flow_ranges
  ADD CONSTRAINT flow_ranges_label_check
    CHECK (label IN ('low', 'running', 'high')),
  ADD CONSTRAINT flow_ranges_reach_id_label_key
    UNIQUE (reach_id, label);

ALTER TABLE user_reach_flow_ranges
  ADD CONSTRAINT user_reach_flow_ranges_label_check
    CHECK (label IN ('low', 'running', 'high')),
  ADD CONSTRAINT user_reach_flow_ranges_user_reach_id_label_key
    UNIQUE (user_reach_id, label);

COMMIT;
