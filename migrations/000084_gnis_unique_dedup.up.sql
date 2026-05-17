-- Remove duplicate rivers that have no gnis_id and no linked reaches/user_reaches.
-- Safe to run multiple times (WHERE clause + cascade protection).
DELETE FROM rivers
WHERE  gnis_id IS NULL
  AND  NOT EXISTS (SELECT 1 FROM reaches     WHERE river_id = rivers.id)
  AND  NOT EXISTS (SELECT 1 FROM user_reaches WHERE river_id = rivers.id);

-- Prevent future duplicate GNIS-keyed rivers.
CREATE UNIQUE INDEX rivers_gnis_id_unique ON rivers (gnis_id) WHERE gnis_id IS NOT NULL;
