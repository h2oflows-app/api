-- web#354 A1 rollback. Best-effort SCHEMA restore only, mirroring the
-- 000143/000144 down conventions — NOT a data-fidelity guarantee. Three
-- losses are permanent and cannot be undone by this file:
--   * calendar_runs.plan_id linkage (which event a run belonged to) —
--     dropped in up step 2; restored below as a nullable, all-NULL column.
--   * events deleted by up step 3 (type='personal') — gone for good.
--   * per-row type/visibility values on surviving events — up steps 4/5
--     dropped the columns entirely; restored below with defaults only, not
--     per-row history.
-- Reverses up steps in opposite order: 5, 4, (3 unrecoverable, skipped),
-- 2, 1.

-- reverse step 5: visibility.
CREATE TYPE plan_visibility AS ENUM ('public', 'private');
ALTER TABLE events ADD COLUMN visibility plan_visibility NOT NULL DEFAULT 'private';

-- reverse step 4: type.
CREATE TYPE plan_type AS ENUM ('personal', 'festival', 'race', 'cruise');
ALTER TABLE events ADD COLUMN type plan_type NOT NULL DEFAULT 'personal';

-- reverse step 3: the DELETE is unrecoverable — no-op here (documented
-- above).

-- reverse step 2: decouple (best-effort; linkage unrecoverable).
DROP INDEX calendar_runs_owner_date_idx;
-- Nullable, all NULL — cannot restore historical values, the NOT NULL
-- constraint, or the ON DELETE CASCADE FK (its target event may no longer
-- exist post-step-3 delete anyway).
ALTER TABLE calendar_runs ADD COLUMN plan_id UUID;

-- reverse step 1: renames.
ALTER INDEX events_owner_dates_idx       RENAME TO plans_owner_dates_idx;
ALTER INDEX calendar_runs_owner_idx      RENAME TO plan_runs_owner_idx;
ALTER INDEX calendar_runs_source_uk      RENAME TO plan_runs_source_uk;
ALTER INDEX calendar_runs_paddled_idx    RENAME TO plan_runs_paddled_idx;
ALTER INDEX calendar_runs_reach_idx      RENAME TO plan_runs_reach_idx;
ALTER INDEX calendar_runs_discover_idx   RENAME TO plan_runs_discover_idx;
ALTER INDEX calendar_runs_meetup_rapid_idx  RENAME TO plan_runs_meetup_rapid_idx;
ALTER INDEX calendar_runs_meetup_access_idx RENAME TO plan_runs_meetup_access_idx;

ALTER TABLE calendar_runs RENAME TO plan_runs;
ALTER TABLE events        RENAME TO plans;
