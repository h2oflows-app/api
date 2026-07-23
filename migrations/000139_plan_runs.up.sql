-- #246: Trip Calendar — plan_runs, the "calendar run" (one dated outing
-- inside a plan). Provenance column source_report_id supports the later
-- 000143 backfill from `reports` and preserves old /reports/{uuid} links.

CREATE TABLE plan_runs (
  id                 UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  plan_id            UUID        NOT NULL REFERENCES plans(id) ON DELETE CASCADE,
  owner_id           TEXT        NOT NULL, -- denorm = plans.owner_id
  user_reach_id      UUID        REFERENCES user_reaches(id) ON DELETE SET NULL,
  slug               TEXT        NOT NULL, -- carries reports.slug for permalink continuity
  run_date           DATE        NOT NULL,
  run_time           TIME,
  sort_order         SMALLINT    NOT NULL DEFAULT 0, -- 2+ runs/day ordering
  gauge_cfs          NUMERIC(12,3),
  flow_band          TEXT, -- free text, NO CHECK — fixes live reports_flow_band_check bug
  flow_color         TEXT, -- resolved p<n> snapshot
  gauge_id           UUID        REFERENCES gauges(id) ON DELETE SET NULL,
  stamped_at         TIMESTAMPTZ,
  paddled            BOOLEAN     NOT NULL DEFAULT FALSE,
  paddled_at         TIMESTAMPTZ, -- 24h-lock anchor
  notes              TEXT, -- was reports.content
  companions         TEXT,
  aw_synced_at       TIMESTAMPTZ,
  source_report_id   UUID, -- migration provenance
  created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  deleted_at         TIMESTAMPTZ,
  UNIQUE (owner_id, slug),
  CHECK (paddled_at IS NULL OR paddled)
);

-- Backfill idempotency: at most one plan_run per source report.
CREATE UNIQUE INDEX plan_runs_source_uk ON plan_runs (source_report_id)
  WHERE source_report_id IS NOT NULL;

CREATE INDEX plan_runs_plan_idx ON plan_runs (plan_id, run_date, sort_order)
  WHERE deleted_at IS NULL;

CREATE INDEX plan_runs_owner_idx ON plan_runs (owner_id, run_date)
  WHERE deleted_at IS NULL;

CREATE INDEX plan_runs_paddled_idx ON plan_runs (owner_id, run_date DESC)
  WHERE paddled AND deleted_at IS NULL;

CREATE INDEX plan_runs_reach_idx ON plan_runs (user_reach_id, run_date DESC)
  WHERE user_reach_id IS NOT NULL AND deleted_at IS NULL;
