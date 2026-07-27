-- web#354 A2: run_invites. Replaces plan_members entirely (F2,
-- issue-authorized DESTRUCTIVE — REWORK_354_PLAN.md §2 step 6 / §1 DDL).
-- Ships in the SAME api deploy as invites.go's rework (rename-style
-- atomicity applies to this DROP too — plan_members existing anywhere the
-- instant this commits would 500 every invite/crew endpoint the moment it
-- runs, same as A1's table RENAMEs). Fresh table, keyed to run_id only — no
-- plan_id/event_id (invites are Run-scoped per §1; the plan/event context
-- plan_members carried through the A1 bridge window is gone for good).
--
-- Reuses the existing plan_member_origin/plan_member_status enums (§1: "kept
-- as-is, opaque, referenced only by run_invites") — no new enum, no
-- migration-time cast needed.
--
-- Data wipe is user-approved (F2, "go" 2026-07-26) — every existing
-- invite/crew-RSVP row is gone. One transaction (golang-migrate wraps each
-- file), per existing 000143/000144/000145 convention (no explicit
-- BEGIN/COMMIT).

DROP TABLE plan_members;

CREATE TABLE run_invites (
  id                 UUID                PRIMARY KEY DEFAULT gen_random_uuid(),
  run_id             UUID                NOT NULL REFERENCES calendar_runs(id) ON DELETE CASCADE,
  member_owner_id    TEXT                REFERENCES user_profiles(owner_id) ON DELETE CASCADE, -- NULL until email invite resolves
  invite_email       TEXT, -- lowercased
  invite_handle      TEXT, -- snapshot
  invited_by         TEXT,
  origin             plan_member_origin  NOT NULL,
  status             plan_member_status  NOT NULL,
  invite_token_hash  TEXT, -- SHA-256, reuse auth.APIKey hash shape
  message            TEXT,
  dismissed_at       TIMESTAMPTZ, -- dismiss keeps row in feed
  responded_at       TIMESTAMPTZ,
  created_at         TIMESTAMPTZ         NOT NULL DEFAULT NOW(),
  updated_at         TIMESTAMPTZ         NOT NULL DEFAULT NOW(),
  CHECK (member_owner_id IS NOT NULL OR invite_email IS NOT NULL)
);

CREATE UNIQUE INDEX run_invites_owner_uk ON run_invites (run_id, member_owner_id)
  WHERE member_owner_id IS NOT NULL;

CREATE UNIQUE INDEX run_invites_email_uk ON run_invites (run_id, lower(invite_email))
  WHERE invite_email IS NOT NULL AND member_owner_id IS NULL;

CREATE INDEX run_invites_feed_idx ON run_invites (member_owner_id, status);
CREATE INDEX run_invites_run_idx  ON run_invites (run_id, status);

CREATE INDEX run_invites_token_idx ON run_invites (invite_token_hash)
  WHERE invite_token_hash IS NOT NULL;
