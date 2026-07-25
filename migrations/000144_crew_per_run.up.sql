-- #246 A7: crew/RSVP remodel — PER-RUN, not per-plan (product decision,
-- IMPLEMENTATION_PLAN.md §6 "REVISED 2026-07-25"). A plan is just the
-- container; each plan_run carries its own crew cap, and invites/join
-- requests RSVP per run so the host knows headcount for a specific
-- trip/run, not the whole multi-day plan.

-- (a) plan_runs gains looking_for_crew/max_crew, copied down from the
-- parent plan wherever it was set (a plan with looking_for_crew=true had
-- exactly one max_crew value for the whole plan under the old model; every
-- one of its runs inherits that same cap under the new one — the closest
-- non-destructive interpretation of "move the flag down a level").
ALTER TABLE plan_runs
  ADD COLUMN looking_for_crew BOOLEAN NOT NULL DEFAULT FALSE,
  ADD COLUMN max_crew         INT,
  ADD CONSTRAINT plan_runs_max_crew_check CHECK (max_crew IS NULL OR max_crew > 0),
  ADD CONSTRAINT plan_runs_crew_flag_check CHECK (NOT looking_for_crew OR max_crew IS NOT NULL);

UPDATE plan_runs pr
SET looking_for_crew = TRUE,
    max_crew          = p.max_crew
FROM plans p
WHERE p.id = pr.plan_id
  AND p.looking_for_crew
  AND p.max_crew IS NOT NULL;

CREATE INDEX plan_runs_discover_idx ON plan_runs (run_date)
  WHERE looking_for_crew AND deleted_at IS NULL;

-- (b) plans loses looking_for_crew/max_crew — plans_discover_idx depended on
-- looking_for_crew, so it goes first (superseded by plan_runs_discover_idx
-- above; discover.go now gates on plan_runs, not plans).
DROP INDEX plans_discover_idx;

ALTER TABLE plans
  DROP COLUMN looking_for_crew,
  DROP COLUMN max_crew;

-- (c) plan_members becomes run-scoped for BOTH invite and request origins.
-- Drop the old plan-scoped-only partial uniques...
DROP INDEX plan_members_owner_uk;
DROP INDEX plan_members_email_uk;

-- ...add run-scoped uniques (the new common case: every invite/request fans
-- out to one row per targeted run)...
CREATE UNIQUE INDEX plan_members_run_owner_uk ON plan_members (plan_run_id, member_owner_id)
  WHERE member_owner_id IS NOT NULL AND plan_run_id IS NOT NULL;

CREATE UNIQUE INDEX plan_members_run_email_uk ON plan_members (plan_run_id, lower(invite_email))
  WHERE invite_email IS NOT NULL AND member_owner_id IS NULL AND plan_run_id IS NOT NULL;

-- ...and KEEP plan-level uniqueness for the one surviving plan_run_id IS NULL
-- case: a plan with zero live runs, whose invites/requests have nothing to
-- fan out to and stay plan-level (membership rule, package comment in
-- internal/handlers/plans.go).
CREATE UNIQUE INDEX plan_members_plan_owner_uk ON plan_members (plan_id, member_owner_id)
  WHERE member_owner_id IS NOT NULL AND plan_run_id IS NULL;

CREATE UNIQUE INDEX plan_members_plan_email_uk ON plan_members (plan_id, lower(invite_email))
  WHERE invite_email IS NOT NULL AND member_owner_id IS NULL AND plan_run_id IS NULL;

CREATE INDEX plan_members_run_status_idx ON plan_members (plan_run_id, status)
  WHERE plan_run_id IS NOT NULL;

-- Fan out every EXISTING plan-level row (plan_run_id IS NULL) to one row per
-- live run of its plan, copying status/origin/email/handle/token_hash/
-- dismissed_at — EXCEPT rows whose plan has zero live runs, which stay
-- plan-level (documented rule above). Split into two INSERTs (owner-bound
-- vs email-bound) so each one's ON CONFLICT target matches exactly one of
-- the new partial unique indexes above; pilot-scale data (a handful of test
-- rows per #246 A7 task brief) makes a same-run double-invite collision
-- unlikely, but ON CONFLICT DO NOTHING keeps this migration idempotent and
-- safe regardless.
INSERT INTO plan_members (
  plan_id, member_owner_id, invite_email, invite_handle, invited_by,
  origin, status, plan_run_id, invite_token_hash, message,
  dismissed_at, responded_at, created_at, updated_at
)
SELECT
  pm.plan_id, pm.member_owner_id, pm.invite_email, pm.invite_handle, pm.invited_by,
  pm.origin, pm.status, pr.id, pm.invite_token_hash, pm.message,
  pm.dismissed_at, pm.responded_at, pm.created_at, pm.updated_at
FROM plan_members pm
JOIN plan_runs pr ON pr.plan_id = pm.plan_id AND pr.deleted_at IS NULL
WHERE pm.plan_run_id IS NULL AND pm.member_owner_id IS NOT NULL
ON CONFLICT (plan_run_id, member_owner_id) WHERE member_owner_id IS NOT NULL AND plan_run_id IS NOT NULL
DO NOTHING;

INSERT INTO plan_members (
  plan_id, member_owner_id, invite_email, invite_handle, invited_by,
  origin, status, plan_run_id, invite_token_hash, message,
  dismissed_at, responded_at, created_at, updated_at
)
SELECT
  pm.plan_id, pm.member_owner_id, pm.invite_email, pm.invite_handle, pm.invited_by,
  pm.origin, pm.status, pr.id, pm.invite_token_hash, pm.message,
  pm.dismissed_at, pm.responded_at, pm.created_at, pm.updated_at
FROM plan_members pm
JOIN plan_runs pr ON pr.plan_id = pm.plan_id AND pr.deleted_at IS NULL
WHERE pm.plan_run_id IS NULL AND pm.member_owner_id IS NULL
ON CONFLICT (plan_run_id, lower(invite_email)) WHERE invite_email IS NOT NULL AND member_owner_id IS NULL AND plan_run_id IS NOT NULL
DO NOTHING;

-- Delete the original plan-level rows that WERE fanned out above (i.e. only
-- those whose plan has >=1 live run — a runless plan's rows were never
-- touched by the INSERTs and must survive here too).
DELETE FROM plan_members pm
WHERE pm.plan_run_id IS NULL
  AND EXISTS (SELECT 1 FROM plan_runs pr WHERE pr.plan_id = pm.plan_id AND pr.deleted_at IS NULL);
