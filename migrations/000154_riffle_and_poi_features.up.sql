-- Two new run-feature types (web#413).
--
-- Riffle  → rapids. A riffle is a rapid-shaped thing that isn't really a rapid:
--           the quick shallow water where a tributary joins a main stem. It
--           belongs on the flowline like every other rapids row, so it gets a
--           boolean discriminator alongside is_surf_wave / is_permanent_hazard
--           rather than a new table or an enum. That keeps the existing pattern
--           (a rapids row is a rapid unless a flag says otherwise) and means
--           every existing row is a valid riffle=false without a backfill.
--
-- POI     → reach_access. Petroglyphs, a side hike, a rock formation: something
--           you look AT rather than paddle THROUGH. In this schema the split is
--           river feature → rapids, off-river feature → reach_access (which
--           already holds camp / parking / boat_ramp / intermediate), so a POI
--           is an access_type, not a rapids flag. It also means a POI does not
--           snap to the centerline, which is right — the rock formation is on
--           the bank, not in the channel.

ALTER TABLE rapids
  ADD COLUMN IF NOT EXISTS is_riffle BOOLEAN NOT NULL DEFAULT false;

COMMENT ON COLUMN rapids.is_riffle IS
  'Minor rapid-like feature (typically a tributary confluence). Mutually exclusive with is_surf_wave / is_permanent_hazard by convention, not by constraint.';

ALTER TABLE reach_access DROP CONSTRAINT reach_access_access_type_check;
ALTER TABLE reach_access ADD CONSTRAINT reach_access_access_type_check
  CHECK (access_type = ANY (ARRAY[
    'put_in', 'take_out', 'shuttle_drop', 'intermediate', 'camp', 'parking', 'boat_ramp', 'poi'
  ]));
