-- Per-run elevation + gradient, replacing the put_in_lng "west=upstream"
-- sort heuristic (web#386) with a direction-agnostic basis: elevation.
--
-- Per-run rather than reusing gauges.elevation_ft: multiple runs commonly
-- share one gauge (a shared elevation would tie them, giving no ordering),
-- and a gauge can sit miles from the actual put-in. Only 92 of 135 prod
-- runs even have a gauge carrying elevation, vs every run having its own
-- put_in/take_out coordinate to query.
--
-- put_in_elevation_ft / take_out_elevation_ft: NUMERIC(8,1), matching
-- gauges.elevation_ft's type exactly (mig 000054) — same USGS-sourced
-- feet-with-one-decimal shape, just resolved per-run via the USGS Elevation
-- Point Query Service (internal/elevation) instead of per-gauge via alt_va.
--
-- gradient_fpm: NUMERIC(6,1), matching the retired reaches.gradient_fpm
-- (mig 000043) — same concept (average drop in feet per mile), lost when
-- runs-unify dropped the reaches table. Computed as
-- (put_in_elevation_ft - take_out_elevation_ft) / centerline river-miles;
-- see internal/handlers/elevation_gradient.go.
--
-- All three nullable: an EPQS lookup failure, or (for gradient) a missing
-- centerline, must not block a run from being created or updated. No
-- backfill here — every existing row starts NULL; cmd/backfill-run-elevation
-- populates all existing rows as a separate, re-runnable step (data
-- backfills don't ride inside schema migrations in this repo — see
-- cmd/backfill-run-state for the established pattern).
ALTER TABLE user_reaches ADD COLUMN put_in_elevation_ft NUMERIC(8,1);
ALTER TABLE user_reaches ADD COLUMN take_out_elevation_ft NUMERIC(8,1);
ALTER TABLE user_reaches ADD COLUMN gradient_fpm NUMERIC(6,1);

COMMENT ON COLUMN user_reaches.put_in_elevation_ft IS 'Altitude in feet at the run''s put-in (USGS Elevation Point Query Service). Used for upstream-to-downstream sorting of runs — see gauges.elevation_ft.';
COMMENT ON COLUMN user_reaches.take_out_elevation_ft IS 'Altitude in feet at the run''s take-out (USGS Elevation Point Query Service).';
COMMENT ON COLUMN user_reaches.gradient_fpm IS 'Average gradient in feet per mile: (put_in_elevation_ft - take_out_elevation_ft) / centerline length in miles. NULL unless both elevations and a centerline are present.';
