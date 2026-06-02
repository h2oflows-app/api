-- Migration 106: backfill watchlist rows pointing at curated reaches into user-owned forks.
--
-- For every user_watchlists row where reach_slug exists in reaches (curated) but
-- not in user_reaches for that user, fork the curated reach into the user's
-- namespace (centerline, flow_ranges 5-tier → 3-tier mapping, original_*) and
-- rewrite the watchlist row to point at the new fork slug.
--
-- Idempotent: skips rows already pointing at user-owned reaches. Safe to re-run.

DO $$
DECLARE
    w RECORD;
    src RECORD;
    new_slug TEXT;
    new_id UUID;
    base_slug TEXT;
    i INTEGER;
    candidate_slug TEXT;
BEGIN
    -- Find every watchlist row needing a fork.
    FOR w IN
        SELECT DISTINCT uw.user_id, uw.reach_slug
        FROM user_watchlists uw
        WHERE uw.reach_slug IS NOT NULL
          AND EXISTS (SELECT 1 FROM reaches r WHERE r.slug = uw.reach_slug)
          AND NOT EXISTS (
              SELECT 1 FROM user_reaches ur
              WHERE ur.owner_id = uw.user_id AND ur.slug = uw.reach_slug
          )
    LOOP
        SELECT
            r.id, r.name, r.common_name, r.river_id, r.river_name,
            r.class_min, r.class_max, r.primary_gauge_id,
            ST_X(r.start_point::geometry)   AS put_in_lng,
            ST_Y(r.start_point::geometry)   AS put_in_lat,
            ST_X(r.end_point::geometry)     AS take_out_lng,
            ST_Y(r.end_point::geometry)     AS take_out_lat,
            ST_AsGeoJSON(r.centerline::geometry) AS centerline_geojson
        INTO src
        FROM reaches r
        WHERE r.slug = w.reach_slug;

        -- Derive unique slug per (owner, slug).
        base_slug := COALESCE(NULLIF(src.common_name, ''), src.name);
        base_slug := regexp_replace(lower(base_slug), '[^a-z0-9]+', '-', 'g');
        base_slug := regexp_replace(base_slug, '(^-|-$)', '', 'g');
        IF base_slug = '' THEN
            base_slug := 'fork';
        END IF;
        candidate_slug := base_slug;
        FOR i IN 2..50 LOOP
            EXIT WHEN NOT EXISTS (
                SELECT 1 FROM user_reaches WHERE owner_id = w.user_id AND slug = candidate_slug
            );
            candidate_slug := base_slug || '-' || i;
        END LOOP;
        new_slug := candidate_slug;

        INSERT INTO user_reaches
            (owner_id, slug, name, river_id, river_name,
             put_in, take_out,
             class_min, class_max, primary_gauge_id,
             forked_from_reach_id, original_forked_at)
        VALUES
            (w.user_id, new_slug,
             COALESCE(NULLIF(src.common_name, ''), src.name),
             src.river_id, src.river_name,
             ST_SetSRID(ST_MakePoint(src.put_in_lng,   src.put_in_lat),   4326)::geography,
             ST_SetSRID(ST_MakePoint(src.take_out_lng, src.take_out_lat), 4326)::geography,
             src.class_min, src.class_max, src.primary_gauge_id,
             src.id, NOW())
        RETURNING id INTO new_id;

        IF src.centerline_geojson IS NOT NULL THEN
            UPDATE user_reaches
            SET centerline = ST_GeomFromGeoJSON(src.centerline_geojson)::geography
            WHERE id = new_id;
        END IF;

        INSERT INTO user_reach_flow_ranges (user_reach_id, label, min_value, max_value)
        SELECT new_id,
               CASE label
                   WHEN 'low_runnable'  THEN 'low'
                   WHEN 'runnable'      THEN 'running'
                   WHEN 'high_runnable' THEN 'high'
               END,
               min_value, max_value
        FROM flow_ranges
        WHERE reach_id = src.id
          AND label IN ('low_runnable','runnable','high_runnable')
        ON CONFLICT (user_reach_id, label) DO NOTHING;

        -- Repoint every watchlist row for this (user, old_slug) → new_slug.
        UPDATE user_watchlists
        SET reach_slug = new_slug
        WHERE user_id = w.user_id AND reach_slug = w.reach_slug;
    END LOOP;
END $$;
