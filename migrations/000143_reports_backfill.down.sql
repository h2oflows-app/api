-- #246 A6 rollback: provenance-driven, NEVER slug-LIKE (a slug-LIKE
-- 'log-%' delete would also nuke user-created plans that happen to share
-- the naming convention — plan section 3, critic #12).
--
-- Capture the candidate plan ids (those that are the parent of at least one
-- backfilled plan_run) BEFORE deleting the plan_runs rows — the provenance
-- link (plan_runs.source_report_id) only exists on the rows we're about to
-- delete, so it must be read first.
CREATE TEMP TABLE _backfill_plan_ids AS
SELECT DISTINCT plan_id FROM plan_runs WHERE source_report_id IS NOT NULL;

DELETE FROM plan_runs WHERE source_report_id IS NOT NULL;

-- Only delete a candidate plan if it is now EMPTY of plan_runs. Guards
-- against destroying real user data: between UP and this DOWN running, a
-- user may have un-tombstoned / added to the same 'log-{date}' plan via
-- POST /plan-runs/{id}/log-mine or POST /me/nudge/confirm (both reuse the
-- exact same day-plan natural key, per findOrCreatePaddledLog) — those
-- native (non-backfill) rows have source_report_id IS NULL and survive the
-- DELETE above, so the plan they live in must survive too.
DELETE FROM plans
WHERE id IN (SELECT plan_id FROM _backfill_plan_ids)
  AND NOT EXISTS (SELECT 1 FROM plan_runs pr WHERE pr.plan_id = plans.id);

DROP TABLE _backfill_plan_ids;
