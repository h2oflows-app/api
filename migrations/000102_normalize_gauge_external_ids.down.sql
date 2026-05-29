-- Down: re-apply prefix for rows that were renamed in-place.
-- NOTE: merged duplicate rows (canonical already existed) cannot be restored.
-- This down migration only restores simple renames.

DO $$
DECLARE
    r RECORD;
BEGIN
    FOR r IN
        SELECT id, external_id, source
        FROM gauges
        WHERE upper(external_id) NOT LIKE upper(source) || '-%'
    LOOP
        -- Re-add prefix only if the gauge was from a source that uses prefixed IDs
        -- and the external_id looks like a bare station number (no other separator).
        -- Conservative: only reverse if external_id contains no '-' already.
        IF r.source IN ('usgs', 'dwr') AND position('-' IN r.external_id) = 0 THEN
            UPDATE gauges
            SET external_id = upper(r.source) || '-' || r.external_id
            WHERE id = r.id;
        END IF;
    END LOOP;
END $$;
