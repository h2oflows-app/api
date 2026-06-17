-- 5c.E additive: add user_reach_id FK to reach_flow_band_overrides pointing
-- at the h2oflows sentinel twin for each curated reach.
-- reach_id column remains until migration 000122 (final DROP TABLE reaches).

ALTER TABLE reach_flow_band_overrides
  ADD COLUMN user_reach_id UUID REFERENCES user_reaches(id) ON DELETE CASCADE;

UPDATE reach_flow_band_overrides o
SET user_reach_id = ur.id
FROM reaches r
JOIN user_reaches ur
  ON ur.slug = r.slug
 AND ur.owner_id = '00000000-0000-0000-0000-000000000001'
 AND ur.deleted_at IS NULL
WHERE r.id = o.reach_id;

ALTER TABLE reach_flow_band_overrides
  ADD CONSTRAINT reach_flow_band_overrides_user_reach_user_id_key
  UNIQUE (user_id, user_reach_id);

CREATE INDEX reach_flow_band_overrides_user_reach_idx
  ON reach_flow_band_overrides (user_reach_id);
