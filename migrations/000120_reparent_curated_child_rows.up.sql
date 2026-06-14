-- Runs-unification Phase 2: reparent curated child rows onto their h2oflows
-- user_reaches twin. Mig 110 copied every curated reach into user_reaches as an
-- h2oflows-owned row (matched by slug). This moves the rich child rows
-- (rapids, reach_access, hazards, reach_conditions, reports) from the curated
-- reach to that twin so the unified user_reaches model carries the meta.
--
-- Reparent (move), not copy: the XOR/num_nonnulls parent checks forbid both
-- parents being set, so we set user_reach_id and NULL reach_id in one update.
-- Reversible via the same slug join (see .down.sql).
--
-- Only rows whose curated reach has a twin move. Curated reaches created after
-- mig 110 by the still-live admin editor have no twin yet; their child rows
-- stay on reach_id until Phase 4 repoints authoring and backfills twins.

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
  SET user_reach_id = ur.id, reach_id = NULL
  FROM reaches r
  JOIN user_reaches ur ON ur.slug = r.slug AND ur.owner_id = h2o
  WHERE c.reach_id = r.id;

  UPDATE reach_access c
  SET user_reach_id = ur.id, reach_id = NULL
  FROM reaches r
  JOIN user_reaches ur ON ur.slug = r.slug AND ur.owner_id = h2o
  WHERE c.reach_id = r.id;

  UPDATE hazards c
  SET user_reach_id = ur.id, reach_id = NULL
  FROM reaches r
  JOIN user_reaches ur ON ur.slug = r.slug AND ur.owner_id = h2o
  WHERE c.reach_id = r.id;

  UPDATE reach_conditions c
  SET user_reach_id = ur.id, reach_id = NULL
  FROM reaches r
  JOIN user_reaches ur ON ur.slug = r.slug AND ur.owner_id = h2o
  WHERE c.reach_id = r.id;

  UPDATE reports c
  SET user_reach_id = ur.id, reach_id = NULL
  FROM reaches r
  JOIN user_reaches ur ON ur.slug = r.slug AND ur.owner_id = h2o
  WHERE c.reach_id = r.id;
END $$;
