-- Best-effort restore: re-add column + index. Existing data cannot be recovered.

ALTER TABLE reports ADD COLUMN IF NOT EXISTS hazard_warning TEXT;
CREATE INDEX IF NOT EXISTS reports_hazard_idx ON reports (reach_id) WHERE hazard_warning IS NOT NULL;
