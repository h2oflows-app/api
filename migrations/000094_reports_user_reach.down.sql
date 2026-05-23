DROP INDEX IF EXISTS reports_user_reach_idx;
ALTER TABLE reports DROP CONSTRAINT IF EXISTS reports_run_source_check;
ALTER TABLE reports
  DROP COLUMN IF EXISTS user_reach_id,
  DROP COLUMN IF EXISTS deleted_at;
ALTER TABLE reports ALTER COLUMN reach_id SET NOT NULL;
