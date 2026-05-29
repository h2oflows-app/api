-- Normalize gauge external_ids: strip "USGS-" / "DWR-" prefix that was stored
-- raw in some insert paths (SetGauge handler did not call normalizeExternalID).
--
-- For each prefixed row:
--   1. If a canonical (stripped) row already exists → repoint all FKs, then delete prefixed row.
--   2. If no canonical row exists → rename external_id in-place.

DO $$
DECLARE
    r RECORD;
    canonical_id UUID;
    stripped TEXT;
BEGIN
    -- Find all gauges whose external_id starts with the source prefix (e.g. USGS-, DWR-).
    FOR r IN
        SELECT id, external_id, source,
               CASE
                   WHEN upper(external_id) LIKE upper(source) || '-%'
                   THEN substring(external_id FROM length(source) + 2)
               END AS stripped_id
        FROM gauges
        WHERE upper(external_id) LIKE upper(source) || '-%'
    LOOP
        stripped := r.stripped_id;
        IF stripped IS NULL OR stripped = '' THEN
            CONTINUE;
        END IF;

        SELECT id INTO canonical_id
        FROM gauges
        WHERE external_id = stripped AND source = r.source AND id != r.id;

        IF canonical_id IS NOT NULL THEN
            -- Canonical already exists — repoint all FK tables, then delete prefixed row.

            -- gauge_readings: ON DELETE CASCADE, so must repoint before delete.
            -- Skip rows that would collide on (gauge_id, timestamp) unique constraint.
            UPDATE gauge_readings SET gauge_id = canonical_id
            WHERE gauge_id = r.id
              AND NOT EXISTS (
                  SELECT 1 FROM gauge_readings gr2
                  WHERE gr2.gauge_id = canonical_id AND gr2.timestamp = gauge_readings.timestamp
              );
            DELETE FROM gauge_readings WHERE gauge_id = r.id;

            -- flow_ranges: ON DELETE CASCADE.
            UPDATE flow_ranges SET gauge_id = canonical_id WHERE gauge_id = r.id;

            -- gauge_reach_associations: ON DELETE CASCADE, unique on (gauge_id, reach_id).
            UPDATE gauge_reach_associations SET gauge_id = canonical_id
            WHERE gauge_id = r.id
              AND NOT EXISTS (
                  SELECT 1 FROM gauge_reach_associations gra2
                  WHERE gra2.gauge_id = canonical_id AND gra2.reach_id = gauge_reach_associations.reach_id
              );
            DELETE FROM gauge_reach_associations WHERE gauge_id = r.id;

            -- user_watchlists: ON DELETE CASCADE, unique on (user_id, gauge_id).
            UPDATE user_watchlists SET gauge_id = canonical_id
            WHERE gauge_id = r.id
              AND NOT EXISTS (
                  SELECT 1 FROM user_watchlists wl2
                  WHERE wl2.gauge_id = canonical_id AND wl2.user_id = user_watchlists.user_id
              );
            DELETE FROM user_watchlists WHERE gauge_id = r.id;

            -- custom_gauge_inputs: ON DELETE RESTRICT — repoint all before delete.
            UPDATE custom_gauge_inputs SET gauge_id = canonical_id WHERE gauge_id = r.id;

            -- reaches.primary_gauge_id: ON DELETE SET NULL — repoint for continuity.
            UPDATE reaches SET primary_gauge_id = canonical_id WHERE primary_gauge_id = r.id;

            -- user_reaches.primary_gauge_id: ON DELETE SET NULL — repoint.
            UPDATE user_reaches SET primary_gauge_id = canonical_id WHERE primary_gauge_id = r.id;

            -- Safe to delete now.
            DELETE FROM gauges WHERE id = r.id;
        ELSE
            -- No canonical row — just strip the prefix in-place.
            UPDATE gauges SET external_id = stripped WHERE id = r.id;
        END IF;
    END LOOP;
END $$;
