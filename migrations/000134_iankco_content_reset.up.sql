-- #314 data migration (h2oflows-app/web#314, step 2 of 2 — DESTRUCTIVE).
-- Depends on 000130-000132 (is_special/public_on_map flags, free-form roles,
-- iankco already granted the h2oflows role).
--
-- 1. HARD-DELETE all personal runs owned by iankco and iank-tc (product
--    decision: hard delete, not tombstone). run_upvotes has an ON DELETE
--    RESTRICT FK (mig 000113) so votes are deleted explicitly first; every
--    other child table cascades; forked_from_user_reach_id and watchlist
--    reference rows are ON DELETE SET NULL (migs 000093/000114) so downstream
--    forks/watchlists survive with lineage cleared.
-- 2. BULK-FORK every live h2oflows run to iankco: full metadata + geometry +
--    centerline + flow bands + features, with fork lineage/attribution,
--    visibility='public', feature rows re-stamped data_source='community'
--    (survives KML re-import, see kmlimport.clearImportDataForTarget).
--
-- Idempotent-ish: step 2 skips slugs iankco already has (fresh after step 1;
-- guard protects a re-run).

-- ── 1. Hard-delete iankco + iank-tc personal runs ───────────────────────────
WITH doomed AS (
    SELECT ur.id
    FROM user_reaches ur
    JOIN user_profiles up ON up.owner_id = ur.owner_id
    WHERE up.handle IN ('iankco', 'iank-tc')
)
DELETE FROM run_upvotes WHERE user_reach_id IN (SELECT id FROM doomed);

DELETE FROM user_reaches
WHERE owner_id IN (SELECT owner_id FROM user_profiles WHERE handle IN ('iankco', 'iank-tc'));

-- ── 2. Fork all live h2oflows runs → iankco ─────────────────────────────────
-- Parent rows (same slug is free after step 1; ON CONFLICT guards re-runs).
INSERT INTO user_reaches
    (owner_id, slug, name, long_name, river_id, river_name, note,
     put_in, take_out, up_comid, down_comid,
     class_min, class_max, primary_gauge_id, custom_gauge_id, centerline,
     base_label, base_color, river_confirmed, completeness_score,
     forked_from_user_reach_id, original_author_handle, original_author_owner_id,
     original_forked_at, visibility, published_at)
SELECT
     (SELECT owner_id FROM user_profiles WHERE handle = 'iankco'),
     src.slug, src.name, src.long_name, src.river_id, src.river_name, src.note,
     src.put_in, src.take_out, src.up_comid, src.down_comid,
     src.class_min, src.class_max, src.primary_gauge_id, src.custom_gauge_id, src.centerline,
     src.base_label, src.base_color, src.river_confirmed, src.completeness_score,
     src.id, 'h2oflows', src.owner_id,
     NOW(), 'public'::run_visibility, NOW()
FROM user_reaches src
JOIN user_profiles sp ON sp.owner_id = src.owner_id AND sp.handle = 'h2oflows'
WHERE src.deleted_at IS NULL
ON CONFLICT DO NOTHING;

-- Child rows: join fork → source via forked_from_user_reach_id.
INSERT INTO user_reach_flow_ranges (user_reach_id, label, value, color)
SELECT dst.id, fr.label, fr.value, fr.color
FROM user_reaches dst
JOIN user_profiles dp ON dp.owner_id = dst.owner_id AND dp.handle = 'iankco'
JOIN user_reach_flow_ranges fr ON fr.user_reach_id = dst.forked_from_user_reach_id
JOIN user_reaches src ON src.id = dst.forked_from_user_reach_id
JOIN user_profiles sp ON sp.owner_id = src.owner_id AND sp.handle = 'h2oflows'
ON CONFLICT DO NOTHING;

INSERT INTO rapids
    (user_reach_id, name, river_mile, location, class_rating, class_at_low, class_at_high,
     description, portage_description, is_portage_recommended,
     is_surf_wave, is_permanent_hazard, hazard_type, data_source, verified)
SELECT dst.id, rp.name, rp.river_mile, rp.location, rp.class_rating, rp.class_at_low, rp.class_at_high,
     rp.description, rp.portage_description, rp.is_portage_recommended,
     rp.is_surf_wave, rp.is_permanent_hazard, rp.hazard_type, 'community', rp.verified
FROM user_reaches dst
JOIN user_profiles dp ON dp.owner_id = dst.owner_id AND dp.handle = 'iankco'
JOIN rapids rp ON rp.user_reach_id = dst.forked_from_user_reach_id
JOIN user_reaches src ON src.id = dst.forked_from_user_reach_id
JOIN user_profiles sp ON sp.owner_id = src.owner_id AND sp.handle = 'h2oflows'
ON CONFLICT DO NOTHING;

INSERT INTO reach_access
    (user_reach_id, access_type, name, location, directions, road_type,
     parking_spaces, parking_fee, permit_required, permit_info, permit_url,
     seasonal_close_start, seasonal_close_end, notes, parking_location,
     entry_style, data_source, verified)
SELECT dst.id, ra.access_type, ra.name, ra.location, ra.directions, ra.road_type,
     ra.parking_spaces, ra.parking_fee, ra.permit_required, ra.permit_info, ra.permit_url,
     ra.seasonal_close_start, ra.seasonal_close_end, ra.notes, ra.parking_location,
     ra.entry_style, 'community', ra.verified
FROM user_reaches dst
JOIN user_profiles dp ON dp.owner_id = dst.owner_id AND dp.handle = 'iankco'
JOIN reach_access ra ON ra.user_reach_id = dst.forked_from_user_reach_id
JOIN user_reaches src ON src.id = dst.forked_from_user_reach_id
JOIN user_profiles sp ON sp.owner_id = src.owner_id AND sp.handle = 'h2oflows'
ON CONFLICT DO NOTHING;
