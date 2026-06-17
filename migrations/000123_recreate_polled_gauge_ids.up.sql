-- gauge_reach_associations was dropped in 000122 (CASCADE took polled_gauge_ids
-- with it). Recreate the view without that source — user_reaches.primary_gauge_id
-- covers the same associations for both sentinel and user runs.
CREATE OR REPLACE VIEW polled_gauge_ids AS
  SELECT DISTINCT gauge_id FROM custom_gauge_inputs
  UNION
  SELECT DISTINCT primary_gauge_id AS gauge_id
    FROM user_reaches
   WHERE primary_gauge_id IS NOT NULL
  UNION
  SELECT DISTINCT gauge_id
    FROM user_watchlists
   WHERE gauge_id IS NOT NULL;
