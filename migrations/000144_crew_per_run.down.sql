-- #246 A7 rollback. Schema/dev-smoke reversibility, not a perfect
-- data-fidelity guarantee (mirrors 000143 down's documented best-effort
-- restore) — task 10's local-DB up/down/up smoke test is what this is
-- shaped for.

-- (d) Drop the "meet up at" columns FIRST — added on top of the crew
-- remodel above (product request 2026-07-25), so it's the last thing the up
-- migration adds and the first thing rolled back here, mirroring (a)-(c)'s
-- own reverse order below. Column drops auto-drop the indexes and the XOR
-- check constraint that depend solely on these columns.
ALTER TABLE plan_runs
  DROP COLUMN meetup_spot,
  DROP COLUMN meetup_rapid_id,
  DROP COLUMN meetup_access_id;

-- (c) Collapse plan_members rows back toward one plan-level row per
-- (plan_id, member_owner_id-or-email): the up migration's fan-out is only
-- cleanly invertible when every fanned-out group agrees on status/message/
-- etc, which isn't guaranteed once independent per-run accept/decline
-- happens post-migration — so this picks the most-recently-updated row per
-- group as the survivor, NULLs its plan_run_id back out, and drops the
-- rest. Documented lossy edge, same spirit as 000143's "not a perfect
-- data-fidelity guarantee".
CREATE TEMP TABLE _collapse_survivors AS
SELECT DISTINCT ON (plan_id, COALESCE(member_owner_id, lower(invite_email)))
  id
FROM plan_members
WHERE plan_run_id IS NOT NULL
ORDER BY plan_id, COALESCE(member_owner_id, lower(invite_email)), updated_at DESC, id;

DELETE FROM plan_members
WHERE plan_run_id IS NOT NULL
  AND id NOT IN (SELECT id FROM _collapse_survivors);

UPDATE plan_members SET plan_run_id = NULL, updated_at = NOW()
WHERE id IN (SELECT id FROM _collapse_survivors);

DROP TABLE _collapse_survivors;

DROP INDEX plan_members_run_status_idx;
DROP INDEX plan_members_plan_owner_uk;
DROP INDEX plan_members_plan_email_uk;
DROP INDEX plan_members_run_owner_uk;
DROP INDEX plan_members_run_email_uk;

CREATE UNIQUE INDEX plan_members_owner_uk ON plan_members (plan_id, member_owner_id)
  WHERE member_owner_id IS NOT NULL;

CREATE UNIQUE INDEX plan_members_email_uk ON plan_members (plan_id, lower(invite_email))
  WHERE invite_email IS NOT NULL AND member_owner_id IS NULL;

-- (b) Restore plans.looking_for_crew/max_crew — best-effort from MAX(...)
-- over each plan's (still-live) child plan_runs, computed BEFORE those
-- columns are dropped from plan_runs below.
ALTER TABLE plans
  ADD COLUMN looking_for_crew BOOLEAN NOT NULL DEFAULT FALSE,
  ADD COLUMN max_crew         INT;

UPDATE plans p
SET looking_for_crew = TRUE,
    max_crew          = sub.max_crew
FROM (
  SELECT plan_id, MAX(max_crew) AS max_crew
  FROM plan_runs
  WHERE looking_for_crew AND deleted_at IS NULL
  GROUP BY plan_id
) sub
WHERE sub.plan_id = p.id;

ALTER TABLE plans
  ADD CONSTRAINT plans_max_crew_check CHECK (max_crew IS NULL OR max_crew > 0),
  ADD CONSTRAINT plans_crew_flag_check CHECK (NOT looking_for_crew OR max_crew IS NOT NULL);

CREATE INDEX plans_discover_idx ON plans (start_date)
  WHERE visibility = 'public' AND looking_for_crew AND deleted_at IS NULL;

-- (a) Drop plan_runs' per-run crew columns.
DROP INDEX plan_runs_discover_idx;

ALTER TABLE plan_runs
  DROP COLUMN looking_for_crew,
  DROP COLUMN max_crew;
