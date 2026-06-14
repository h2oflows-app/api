-- Reverse Phase 2: move reparented child rows back from the h2oflows
-- user_reaches twin to the curated reach, matched by the same slug join.
-- Only h2oflows-owned twins that still have a matching curated reach are
-- reversed; genuinely user-authored child rows (other owners) are untouched.

DO $$
DECLARE
  h2o TEXT;
BEGIN
  SELECT owner_id INTO h2o
  FROM user_profiles WHERE LOWER(handle) = 'h2oflows' LIMIT 1;
  IF h2o IS NULL THEN
    RAISE EXCEPTION 'h2oflows user profile not found';
  END IF;

  UPDATE rapids c
  SET reach_id = r.id, user_reach_id = NULL
  FROM user_reaches ur
  JOIN reaches r ON r.slug = ur.slug
  WHERE c.user_reach_id = ur.id AND ur.owner_id = h2o;

  UPDATE reach_access c
  SET reach_id = r.id, user_reach_id = NULL
  FROM user_reaches ur
  JOIN reaches r ON r.slug = ur.slug
  WHERE c.user_reach_id = ur.id AND ur.owner_id = h2o;

  UPDATE hazards c
  SET reach_id = r.id, user_reach_id = NULL
  FROM user_reaches ur
  JOIN reaches r ON r.slug = ur.slug
  WHERE c.user_reach_id = ur.id AND ur.owner_id = h2o;

  UPDATE reach_conditions c
  SET reach_id = r.id, user_reach_id = NULL
  FROM user_reaches ur
  JOIN reaches r ON r.slug = ur.slug
  WHERE c.user_reach_id = ur.id AND ur.owner_id = h2o;

  UPDATE reports c
  SET reach_id = r.id, user_reach_id = NULL
  FROM user_reaches ur
  JOIN reaches r ON r.slug = ur.slug
  WHERE c.user_reach_id = ur.id AND ur.owner_id = h2o;
END $$;
