-- Drop hazard_warning column from reports.
-- Field removed in 0.3.0 polish for liability/bad-info reasons; will be
-- reintroduced later with proper UX guardrails (see ROADMAP 3.6).

DROP INDEX IF EXISTS reports_hazard_idx;
ALTER TABLE reports DROP COLUMN IF EXISTS hazard_warning;
