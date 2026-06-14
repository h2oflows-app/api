-- Runs-unification Phase 3a: bring user_reaches to parity with the admin
-- authoring fields on the legacy reaches table, so the /admin/reaches handlers
-- can write/read h2oflows-owned user_reaches instead of curated reaches.
--
-- Gap vs reaches: permit_required, multi_day_days, centerline_source,
-- length_mi (set by centerline sync), gradient_fpm (admin gradient calc),
-- river_order (admin per-river ordering / PatchReach).
-- anchor_comid is intentionally NOT added — on reaches it duplicates start_comid
-- (CreateReach sets anchor_comid = start_comid), so up_comid already covers it.

ALTER TABLE user_reaches
    ADD COLUMN permit_required   BOOLEAN  NOT NULL DEFAULT false,
    ADD COLUMN multi_day_days    INT      NOT NULL DEFAULT 1,
    ADD COLUMN centerline_source TEXT,
    ADD COLUMN length_mi         NUMERIC,
    ADD COLUMN gradient_fpm      NUMERIC,
    ADD COLUMN river_order       SMALLINT;

-- Backfill the h2oflows twins (created by mig 110, before these columns existed)
-- from their curated source reach, matched by slug. Other owners keep defaults.
DO $$
DECLARE
  h2o TEXT;
BEGIN
  SELECT owner_id INTO h2o
  FROM user_profiles WHERE LOWER(handle) = 'h2oflows' LIMIT 1;
  IF h2o IS NULL THEN
    RAISE EXCEPTION 'h2oflows user profile not found';
  END IF;

  UPDATE user_reaches ur
  SET permit_required   = COALESCE(r.permit_required, false),
      multi_day_days    = COALESCE(r.multi_day_days, 1),
      centerline_source = r.centerline_source,
      length_mi         = r.length_mi,
      gradient_fpm      = r.gradient_fpm,
      river_order       = r.river_order
  FROM reaches r
  WHERE r.slug = ur.slug AND ur.owner_id = h2o;
END $$;
