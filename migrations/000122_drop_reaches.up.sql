-- runs-unify 5c.E — irreversible DROP TABLE reaches and all legacy FK columns.
-- GATE: runs only after PRs 5c.D + 5c.E are merged and prod-stable.
--
-- Order:
--   1. Safety checks — abort if live rows still reference reaches
--   2. Collapse forked_from_reach_id → forked_from_user_reach_id
--   3. Drop reach_id from reach_flow_band_overrides (code uses user_reach_id now)
--   4. Drop child reach_id cols (rapids/reach_access/hazards/reach_conditions/reports)
--   5. Drop dead tables (trip_reports/contributions/proximity_events/reach_relationships/
--      reach_embeddings/flow_ranges/gauge_reach_associations)
--   6. Drop dead cols (gauges.reach_id / trips.reach_id / user_reaches.forked_from_reach_id)
--   7. DROP TABLE reaches

BEGIN;

-- ── 1. Safety checks ──────────────────────────────────────────────────────────

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM rapids WHERE reach_id IS NOT NULL) THEN
    RAISE EXCEPTION 'rapids still has reach_id rows — reparent first';
  END IF;
  IF EXISTS (SELECT 1 FROM reach_access WHERE reach_id IS NOT NULL) THEN
    RAISE EXCEPTION 'reach_access still has reach_id rows';
  END IF;
  IF EXISTS (SELECT 1 FROM hazards WHERE reach_id IS NOT NULL) THEN
    RAISE EXCEPTION 'hazards still has reach_id rows';
  END IF;
  IF EXISTS (SELECT 1 FROM reach_conditions WHERE reach_id IS NOT NULL) THEN
    RAISE EXCEPTION 'reach_conditions still has reach_id rows';
  END IF;
  IF EXISTS (SELECT 1 FROM reports WHERE reach_id IS NOT NULL) THEN
    RAISE EXCEPTION 'reports still has reach_id rows';
  END IF;
END $$;

-- ── 2. Collapse forked_from_reach_id → forked_from_user_reach_id ─────────────
-- For user_reaches that forked from a curated reach (forked_from_reach_id IS NOT NULL)
-- but haven't been linked to the twin yet, resolve via slug.

UPDATE user_reaches ur
SET    forked_from_user_reach_id = twin.id,
       forked_from_reach_id      = NULL        -- clear to satisfy fork_single_source check constraint
FROM   reaches r
JOIN   user_reaches twin
       ON twin.slug     = r.slug
      AND twin.owner_id = '00000000-0000-0000-0000-000000000001'
      AND twin.deleted_at IS NULL
WHERE  r.id = ur.forked_from_reach_id
  AND  ur.forked_from_user_reach_id IS NULL;

-- ── 3. reach_flow_band_overrides — drop reach_id col ─────────────────────────

ALTER TABLE reach_flow_band_overrides
  DROP CONSTRAINT IF EXISTS reach_flow_band_overrides_user_id_reach_id_key,
  DROP CONSTRAINT IF EXISTS reach_flow_band_overrides_reach_id_fkey;

DROP INDEX IF EXISTS reach_flow_band_overrides_reach_idx;

ALTER TABLE reach_flow_band_overrides DROP COLUMN IF EXISTS reach_id;

-- ── 4. Drop child reach_id columns ───────────────────────────────────────────

-- rapids
ALTER TABLE rapids
  DROP CONSTRAINT IF EXISTS rapids_parent_xor,
  DROP COLUMN IF EXISTS reach_id;

-- reach_access
ALTER TABLE reach_access
  DROP CONSTRAINT IF EXISTS reach_access_parent_xor,
  DROP COLUMN IF EXISTS reach_id;

-- hazards
ALTER TABLE hazards
  DROP CONSTRAINT IF EXISTS hazards_parent_xor,
  DROP COLUMN IF EXISTS reach_id;

-- reach_conditions
ALTER TABLE reach_conditions
  DROP CONSTRAINT IF EXISTS reach_conditions_parent_xor,
  DROP COLUMN IF EXISTS reach_id;

-- reports (dual-parent — reach_id + user_reach_id; both nullable)
ALTER TABLE reports DROP COLUMN IF EXISTS reach_id;

-- ── 5. Drop dead tables ───────────────────────────────────────────────────────

DROP TABLE IF EXISTS trip_reports;
DROP TABLE IF EXISTS contributions;
DROP TABLE IF EXISTS proximity_events;
DROP TABLE IF EXISTS reach_relationships;
DROP TABLE IF EXISTS reach_embeddings;
DROP TABLE IF EXISTS flow_ranges;
DROP TABLE IF EXISTS gauge_reach_associations;

-- ── 6. Drop dead columns ──────────────────────────────────────────────────────

ALTER TABLE gauges DROP COLUMN IF EXISTS reach_id;
ALTER TABLE trips  DROP COLUMN IF EXISTS reach_id;
ALTER TABLE user_reaches DROP COLUMN IF EXISTS forked_from_reach_id;

-- ── 7. DROP TABLE reaches ─────────────────────────────────────────────────────

DROP TABLE reaches;

COMMIT;
