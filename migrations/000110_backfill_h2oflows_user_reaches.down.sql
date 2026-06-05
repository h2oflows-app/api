-- Remove h2oflows user_reaches that were backfilled from reaches.
-- Only removes rows whose slug matches a curated reach slug (safe rollback).

DO $$
DECLARE
  h2o_owner_id TEXT;
BEGIN
  SELECT owner_id INTO h2o_owner_id
  FROM user_profiles WHERE LOWER(handle) = 'h2oflows' LIMIT 1;

  IF h2o_owner_id IS NULL THEN RETURN; END IF;

  DELETE FROM user_reaches
  WHERE owner_id = h2o_owner_id
    AND slug IN (SELECT slug FROM reaches);
END $$;
