-- Reverse of 000154. Destructive by necessity: down-migrating removes the two
-- feature types, so any row that USES them has to go with them.

ALTER TABLE rapids DROP COLUMN IF EXISTS is_riffle;

-- Rows first, then the constraint — re-adding a CHECK that existing rows
-- violate fails outright, so a POI left behind would block the whole down.
DELETE FROM reach_access WHERE access_type = 'poi';

ALTER TABLE reach_access DROP CONSTRAINT reach_access_access_type_check;
ALTER TABLE reach_access ADD CONSTRAINT reach_access_access_type_check
  CHECK (access_type = ANY (ARRAY[
    'put_in', 'take_out', 'shuttle_drop', 'intermediate', 'camp', 'parking', 'boat_ramp'
  ]));
