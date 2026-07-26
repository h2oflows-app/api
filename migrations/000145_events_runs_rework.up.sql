-- web#354 A1: foundational rename + decouple. `plans` -> `events`,
-- `plan_runs` -> `calendar_runs`; runs no longer belong to a parent event
-- (drop plan_id — "coupled only by date" per REWORK_354_PLAN.md §1); the
-- event-type and visibility concepts are dropped entirely (title carries
-- the semantics; visibility replaced by the per-run paddled/crew/token rule
-- in Go). `plan_members` is UNTOUCHED here — it outlives A1 (see plan §2
-- note); it gets dropped/replaced by run_invites in a later 000146 (A2),
-- not this file. Steps mirror plan §2 order exactly:
--   1. rename tables + cosmetic index renames (FKs auto-repoint)
--   2. decouple: drop the plan_id link (index, FK, column), add the
--      replacement owner+date index
--   3. delete personal container events [F1] -- MUST run before step 4 (it
--      filters on type='personal'); safe now because plan_id/CASCADE is
--      already gone in step 2, so the events' calendar_runs survive
--   4. drop the event-type concept (after step 3 consumes it)
--   5. drop visibility (plans_discover_idx, the only visibility-dependent
--      index, was already dropped in 000144)
-- One transaction (golang-migrate wraps each file), per existing
-- 000143/000144 convention (no explicit BEGIN/COMMIT).

-- 1. Rename tables/indexes first.
ALTER TABLE plans     RENAME TO events;
ALTER TABLE plan_runs RENAME TO calendar_runs;

ALTER INDEX plans_owner_dates_idx       RENAME TO events_owner_dates_idx;
ALTER INDEX plan_runs_owner_idx         RENAME TO calendar_runs_owner_idx;
ALTER INDEX plan_runs_source_uk         RENAME TO calendar_runs_source_uk;
ALTER INDEX plan_runs_paddled_idx       RENAME TO calendar_runs_paddled_idx;
ALTER INDEX plan_runs_reach_idx         RENAME TO calendar_runs_reach_idx;
ALTER INDEX plan_runs_discover_idx      RENAME TO calendar_runs_discover_idx;
ALTER INDEX plan_runs_meetup_rapid_idx  RENAME TO calendar_runs_meetup_rapid_idx;
ALTER INDEX plan_runs_meetup_access_idx RENAME TO calendar_runs_meetup_access_idx;

-- 2. Decouple: drop the parent/child link.
DROP INDEX plan_runs_plan_idx;
-- Unnamed at creation (000139), so Postgres assigned the default
-- <table>_<column>_fkey name; verified via grep against 000139 (no other
-- migration references a different explicit name).
ALTER TABLE calendar_runs DROP CONSTRAINT plan_runs_plan_id_fkey;
ALTER TABLE calendar_runs DROP COLUMN plan_id;

CREATE INDEX calendar_runs_owner_date_idx ON calendar_runs (owner_id, run_date, sort_order)
  WHERE deleted_at IS NULL;

-- 3. Delete container/personal events [DESTRUCTIVE F1, user-approved
-- 2026-07-26]. Includes all 000143 'log-{date}' backfill containers and
-- log-mine/nudge-confirm day-events, plus any real user-named personal
-- events. Safe now: plan_id/CASCADE is already gone (step 2), so the
-- events' calendar_runs rows survive standalone.
DELETE FROM events WHERE type = 'personal';

-- 4. Drop the event-type concept (after step 3, which filtered on it).
-- Surviving festival/race/cruise events become typeless — name carries
-- the semantics.
ALTER TABLE events DROP COLUMN type;
DROP TYPE plan_type;

-- 5. Drop visibility. Replaced by the per-run paddled/crew/token gate in Go
-- (events themselves become owner-only, unconditionally).
ALTER TABLE events DROP COLUMN visibility;
DROP TYPE plan_visibility;

-- Addendum (NOT one of plan §2's 5 listed steps — a necessary corollary of
-- step 2, called out here rather than silently folded in): plan_members.
-- plan_id is NOT NULL, FK'd to events(id) (mig 000140). Every run created
-- from now on is standalone (POST /plan-runs has no parent event —
-- plan_runs.go's old "add a run to this plan" endpoint is removed), so a
-- NEW crew-join-request row against such a run (InviteHandler.JoinRun,
-- internal/handlers/invites.go) has no event to supply here and would
-- violate the NOT NULL constraint. Loosening it to nullable is the minimal
-- schema change that keeps the join/crew flow alive through the A1->A2
-- bridge window (A2's 000146 drops plan_id from plan_members entirely via
-- run_invites) — plan_members otherwise keeps its name, shape, and every
-- other column/constraint untouched, so this does not contradict "steps
-- 1-5 ONLY": it isn't step 6 (no DROP/CREATE of plan_members), just a
-- one-column relaxation forced by step 2. Flagged in the PR/report for
-- review.
ALTER TABLE plan_members ALTER COLUMN plan_id DROP NOT NULL;
