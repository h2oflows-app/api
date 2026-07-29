-- API-1 Invite Sync (Outlook-parity) — see
-- web/design_handoff_calendar_explore/INVITE_SYNC_PLAN.md. Two additive,
-- non-destructive pieces:
--
-- 1. calendar_runs.ics_sequence: RFC 5546 SEQUENCE counter for this run's
--    UID ({run_id}@h2oflows.app, fixed forever). 0 on the initial REQUEST;
--    UpdateRun (API-2) bumps it +1 on every material-field change so
--    Outlook/Google/Apple accept the update instead of ignoring it as a
--    stale duplicate of what they already imported.
-- 2. user_emails: the ONLY stored-address table in the schema (mirrors the
--    plan's "Ground truth": user_profiles has no email column; emails
--    otherwise ride the JWT per request, auth.EmailFromContext). One row
--    per owner_id, best-effort upserted on every authed write that already
--    has a fresh email in hand (POST /plan-runs, findOrCreatePaddledLog,
--    AcceptInvite) — organizer/attendee notifications (notifications.go)
--    resolve against it. No backfill: pre-existing users get a row on
--    their next authed write, not retroactively (plan risk R2, documented,
--    not a bug).
--
-- No enum changes — plan_member_status already has 'declined' (reused for
-- the leave/uninvite flows API-2 adds), dodging the ALTER TYPE ADD VALUE
-- same-transaction gotcha under golang-migrate.

ALTER TABLE calendar_runs ADD COLUMN ics_sequence INT NOT NULL DEFAULT 0;

CREATE TABLE user_emails (
  owner_id   TEXT        PRIMARY KEY REFERENCES user_profiles(owner_id) ON DELETE CASCADE,
  email      TEXT        NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
