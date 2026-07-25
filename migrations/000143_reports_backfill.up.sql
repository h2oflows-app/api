-- #246 A6: reports -> plans/plan_runs backfill (design_handoff_calendar_explore
-- /IMPLEMENTATION_PLAN.md §3). Pure INSERT...SELECT — `reports` is never
-- mutated. Migrates LIVE reports only (deleted_at IS NULL, contract decision
-- #6) — soft-deleted reports stay in `reports` for moderation history.
--
-- Idempotent: safe to run more than once (NOT EXISTS guards below), and
-- compatible with rows PlanHandler.LogMine/NudgeHandler.Confirm's shared
-- findOrCreatePaddledLog helper (internal/handlers/plan_runs.go) may have
-- ALREADY created in prod before this migration deploys — same owner/day
-- 'log-{date}' Personal plans. Step (a) reuses them instead of colliding on
-- plans' UNIQUE(owner_id,slug).
--
-- FK preflight: `plans.owner_id` and `plan_runs.owner_id`-adjacent inserts
-- below both filter to owner_ids present in user_profiles (plans.owner_id
-- has a hard FK there, mig 000138) — this is the same check the deploy
-- runbook's manual pre-flight query performs; filtering it into the
-- migration itself makes 000143 resilient even if that manual step is
-- skipped. The DO block below just surfaces the count for the deploy log.

DO $$
DECLARE
  orphan_count integer;
BEGIN
  SELECT COUNT(*) INTO orphan_count
  FROM reports rp
  WHERE rp.deleted_at IS NULL
    AND NOT EXISTS (SELECT 1 FROM user_profiles up WHERE up.owner_id = rp.owner_id);

  IF orphan_count > 0 THEN
    RAISE NOTICE '#246 A6 backfill (000143): % live report(s) reference an owner_id with no user_profiles row and will be SKIPPED (no plans/plan_runs row created). Runbook preflight query: SELECT DISTINCT owner_id FROM reports WHERE deleted_at IS NULL AND owner_id NOT IN (SELECT owner_id FROM user_profiles);', orphan_count;
  END IF;
END $$;

-- (a) One Personal plan per (owner_id, report_date) over LIVE reports.
--
-- Natural-key guard is `NOT EXISTS ... WHERE p.owner_id=... AND p.slug=...`
-- with NO deleted_at condition — i.e. we key on slug ALONE, regardless of
-- tombstone state. This is deliberate (plan section 3 "think it through"):
-- plans.UNIQUE(owner_id,slug) is non-partial, so if we guarded only on live
-- plans, a day a user has since deleted (tombstoned 'log-{date}') would
-- collide with a 23505 on this INSERT. Un-tombstoning here would be an
-- app-layer decision (that's what findOrCreatePaddledLog does for the
-- log-mine/nudge-confirm paths) — out of scope for a pure SQL migration.
-- So: if 'log-{date}' already exists at all (live OR tombstoned), skip
-- creating a duplicate; step (b) below then ALSO skips attaching reports to
-- a tombstoned day-plan (joins on p.deleted_at IS NULL), keeping (a) and
-- (b) consistent. Net effect, documented: a user who tombstoned a
-- 'log-{date}' plan before this backfill ran keeps that day empty/deleted
-- post-migration; their old reports for that day are not surfaced as
-- calendar entries. Re-creating that day is a normal "add a run" action.
INSERT INTO plans (owner_id, slug, name, type, visibility, start_date, end_date, created_at, updated_at)
SELECT
  rp.owner_id,
  'log-' || rp.report_date::text                       AS slug,
  'Paddle ' || rp.report_date::text                     AS name,
  'personal'::plan_type,
  'public'::plan_visibility,
  rp.report_date,
  rp.report_date,
  MIN(rp.created_at),
  MIN(rp.created_at)
FROM reports rp
WHERE rp.deleted_at IS NULL
  AND EXISTS (SELECT 1 FROM user_profiles up WHERE up.owner_id = rp.owner_id)
  AND NOT EXISTS (
    SELECT 1 FROM plans p
    WHERE p.owner_id = rp.owner_id AND p.slug = 'log-' || rp.report_date::text
  )
GROUP BY rp.owner_id, rp.report_date;

-- (b) + (c) One plan_runs row per LIVE report, plan_id resolved via the
-- join to the day's plan (found or reused above) — folds the "UPDATE
-- plan_runs.plan_id from the day's plan" step from plan section 3 into the
-- INSERT itself (a correlated UPDATE pass afterward would be redundant:
-- the join below already produces the correct plan_id inline, and running
-- it as a single INSERT keeps the whole backfill atomic within this
-- migration's transaction).
--
-- JOIN (not LEFT JOIN) on plans with p.deleted_at IS NULL: a report whose
-- day-plan resolved to a tombstoned 'log-{date}' row (see (a) above) is
-- silently skipped here too — never attached to a dead parent, consistent
-- with the documented (a)/(b) choice.
--
-- Idempotency: plan_runs_source_uk (mig 000139, UNIQUE(source_report_id)
-- WHERE NOT NULL) plus the explicit NOT EXISTS below make re-running this
-- migration a no-op on rows already migrated.
--
-- slug collision guard: plan_runs also has UNIQUE(owner_id,slug), and
-- report slugs are generated the same "{reach-slug}-{date}" shape the app
-- uses for native plan_runs (uniqueRunSlug, plan_runs.go) — a user who
-- logged the SAME reach+date via BOTH the old report flow and the new
-- calendar UI during the transition window (A3 has been live) could already
-- hold a native plan_runs row with that exact slug, which would abort this
-- whole migration file (golang-migrate wraps each file in one tx) on a bare
-- INSERT. The CASE below deterministically re-suffixes with the report's
-- own id (guaranteed unique, so this never needs a second collision check)
-- only when that pre-existing collision is detected.
INSERT INTO plan_runs (
  plan_id, owner_id, user_reach_id, slug, run_date, run_time,
  gauge_cfs, flow_band, notes, paddled, paddled_at, aw_synced_at,
  source_report_id, created_at, updated_at
)
SELECT
  p.id,
  rp.owner_id,
  rp.user_reach_id,
  CASE
    WHEN EXISTS (
      SELECT 1 FROM plan_runs x WHERE x.owner_id = rp.owner_id AND x.slug = rp.slug
    ) THEN rp.slug || '-bf-' || substr(rp.id::text, 1, 8)
    ELSE rp.slug
  END                                                    AS slug,
  rp.report_date,
  rp.report_time,
  rp.flow_cfs,
  rp.flow_band,
  rp.content,
  rp.paddled,
  CASE WHEN rp.paddled THEN rp.created_at ELSE NULL END  AS paddled_at, -- 24h-lock anchor, preserved exactly
  rp.aw_synced_at,
  rp.id                                                   AS source_report_id,
  rp.created_at,
  rp.updated_at
FROM reports rp
JOIN plans p
  ON p.owner_id = rp.owner_id
 AND p.slug = 'log-' || rp.report_date::text
 AND p.deleted_at IS NULL
WHERE rp.deleted_at IS NULL
  AND EXISTS (SELECT 1 FROM user_profiles up WHERE up.owner_id = rp.owner_id)
  AND NOT EXISTS (SELECT 1 FROM plan_runs pr2 WHERE pr2.source_report_id = rp.id);
