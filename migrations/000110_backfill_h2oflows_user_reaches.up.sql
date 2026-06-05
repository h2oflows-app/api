-- Backfill all curated reaches into h2oflows user_reaches.
-- h2oflows admin manages content through user_reaches going forward.
-- reaches table stays as legacy read source; user_reaches is the live data.

DO $$
DECLARE
  h2o_owner_id TEXT;
BEGIN
  SELECT owner_id INTO h2o_owner_id
  FROM user_profiles WHERE LOWER(handle) = 'h2oflows' LIMIT 1;

  IF h2o_owner_id IS NULL THEN
    RAISE EXCEPTION 'h2oflows user profile not found';
  END IF;

  -- Insert user_reaches for each curated reach not already present
  INSERT INTO user_reaches (
    id,
    owner_id,
    slug,
    name,
    long_name,
    river_id,
    river_name,
    put_in,
    take_out,
    centerline,
    up_comid,
    down_comid,
    primary_gauge_id,
    note,
    class_min,
    class_max,
    is_private,
    base_label,
    base_color,
    created_at,
    updated_at
  )
  SELECT
    gen_random_uuid(),
    h2o_owner_id,
    r.slug,
    COALESCE(r.common_name, r.name),  -- common_name → short name field
    r.name,                             -- full section name → long_name
    r.river_id,
    r.river_name,
    r.start_point,
    r.end_point,
    r.centerline,
    r.start_comid,
    r.end_comid,
    r.primary_gauge_id,
    r.description,
    r.class_min,
    r.class_max,
    FALSE,
    r.base_label,
    r.base_color,
    r.created_at,
    NOW()
  FROM reaches r
  WHERE NOT EXISTS (
    SELECT 1 FROM user_reaches ur
    WHERE ur.owner_id = h2o_owner_id AND ur.slug = r.slug
  );

  -- Copy flow_ranges → user_reach_flow_ranges for newly inserted rows
  INSERT INTO user_reach_flow_ranges (user_reach_id, value, label, color)
  SELECT ur.id, fr.value, fr.label, fr.color
  FROM flow_ranges fr
  JOIN reaches r ON r.id = fr.reach_id
  JOIN user_reaches ur ON ur.slug = r.slug AND ur.owner_id = h2o_owner_id
  WHERE NOT EXISTS (
    SELECT 1 FROM user_reach_flow_ranges urfr WHERE urfr.user_reach_id = ur.id
  );

END $$;
