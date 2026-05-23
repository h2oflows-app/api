-- Allow reports on user-created runs (user_reaches) in addition to curated reaches.
ALTER TABLE reports ALTER COLUMN reach_id DROP NOT NULL;

ALTER TABLE reports
  ADD COLUMN user_reach_id UUID REFERENCES user_reaches(id) ON DELETE CASCADE,
  ADD COLUMN deleted_at    TIMESTAMPTZ;

-- Exactly one run source must be set.
ALTER TABLE reports
  ADD CONSTRAINT reports_run_source_check
    CHECK (num_nonnulls(reach_id, user_reach_id) = 1);

CREATE INDEX reports_user_reach_idx ON reports (user_reach_id, report_date DESC)
  WHERE user_reach_id IS NOT NULL;
