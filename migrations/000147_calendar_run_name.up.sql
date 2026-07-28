-- web#354 A4: calendar_runs.name — calendar Runs get their own user-supplied
-- name ("Lower Blue Cruise"), independent of the attached library run's
-- name. name is REQUIRED for both Events (already NOT NULL, mig 000138) and
-- calendar Runs — this migration adds the calendar-run half.
ALTER TABLE calendar_runs ADD COLUMN name TEXT;

-- Backfill, rows with a live user_reach_id: inherit that reach's OWN name
-- (user_reaches.name — the field every handler join in the calendar domain
-- already reads for display: plans.go/plan_runs.go/calendar.go/invites.go/
-- discover.go/og.go/nudges.go all SELECT ur.name, never ur.long_name, for
-- this purpose). user_reaches.name is itself NOT NULL (mig 000071), so this
-- always yields a real value whenever the FK resolves — including a
-- soft-deleted (deleted_at IS NOT NULL) reach, deliberately not filtered out
-- here: the row still exists, still has a name, and ON DELETE SET NULL only
-- fires on a hard delete, so "soft-deleted but still linked" is exactly the
-- FK still resolving.
UPDATE calendar_runs cr
SET name = ur.name
FROM user_reaches ur
WHERE ur.id = cr.user_reach_id AND cr.name IS NULL;

-- Backfill, orphaned rows (user_reach_id IS NULL — never set, or the reach
-- was later hard-deleted, ON DELETE SET NULL per 000139): no library name to
-- inherit. Falls back to the calendar_run's own slug rather than a bare
-- literal 'Run' for every orphan — slug is already NOT NULL, already unique
-- per owner (calendar_runs UNIQUE(owner_id,slug), mig 000139), and carries
-- whatever the row was created from (reach slug, "run", or a
-- source_report_id-derived slug), so it reads as a real (if terse) label
-- instead of colliding en masse in list views the way a constant would.
UPDATE calendar_runs
SET name = slug
WHERE name IS NULL;

ALTER TABLE calendar_runs ALTER COLUMN name SET NOT NULL;
