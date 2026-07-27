-- web#354 A2 rollback. Schema-smoke only (mirrors the 000143/000144/000145
-- down conventions) — NOT a data-fidelity guarantee: every invite/crew-RSVP
-- row created under run_invites is gone the instant the DROP below runs, and
-- there is no plan_id/event_id on run_invites to backfill plan_members' FK
-- from even in principle (invites are Run-scoped only as of A2 — the
-- plan/event context was already gone from run_invites' schema, not just its
-- data).
--
-- Recreates plan_members in EXACTLY its post-A1 shape — reconstructed from
-- THREE migrations (000140's initial CREATE TABLE, 000144's crew-per-run
-- index rework, and 000145's plan_id NOT-NULL-relax addendum), not copied
-- verbatim from any single one of them: 000140's shape alone is stale
-- (000144 dropped two of its unique indexes and replaced them), and 000144's
-- down migration alone would restore the PRE-000144 (single-plan-level,
-- NOT NULL plan_id) shape, not the post-A1 one A2 actually inherited.

DROP TABLE run_invites;

CREATE TABLE plan_members (
  id                 UUID                PRIMARY KEY DEFAULT gen_random_uuid(),
  plan_id            UUID                REFERENCES events(id) ON DELETE CASCADE, -- nullable (000145 addendum)
  member_owner_id    TEXT                REFERENCES user_profiles(owner_id) ON DELETE CASCADE,
  invite_email       TEXT,
  invite_handle      TEXT,
  invited_by         TEXT,
  origin             plan_member_origin  NOT NULL,
  status             plan_member_status  NOT NULL,
  plan_run_id        UUID                REFERENCES calendar_runs(id) ON DELETE CASCADE, -- added 000144
  invite_token_hash  TEXT,
  message            TEXT,
  dismissed_at       TIMESTAMPTZ,
  responded_at       TIMESTAMPTZ,
  created_at         TIMESTAMPTZ         NOT NULL DEFAULT NOW(),
  updated_at         TIMESTAMPTZ         NOT NULL DEFAULT NOW(),
  CHECK (member_owner_id IS NOT NULL OR invite_email IS NOT NULL)
);

-- 000140 survivors — never touched by 000144's index rework below.
CREATE INDEX plan_members_feed_idx ON plan_members (member_owner_id, status);
CREATE INDEX plan_members_plan_idx ON plan_members (plan_id, status);

CREATE INDEX plan_members_token_idx ON plan_members (invite_token_hash)
  WHERE invite_token_hash IS NOT NULL;

-- 000144 crew-per-run rework's run-scoped uniques (the common case: one row
-- per targeted run) plus the plan-scoped fallback uniques (the one surviving
-- plan_run_id IS NULL case: a runless plan/event, membership rule).
CREATE UNIQUE INDEX plan_members_run_owner_uk ON plan_members (plan_run_id, member_owner_id)
  WHERE member_owner_id IS NOT NULL AND plan_run_id IS NOT NULL;

CREATE UNIQUE INDEX plan_members_run_email_uk ON plan_members (plan_run_id, lower(invite_email))
  WHERE invite_email IS NOT NULL AND member_owner_id IS NULL AND plan_run_id IS NOT NULL;

CREATE UNIQUE INDEX plan_members_plan_owner_uk ON plan_members (plan_id, member_owner_id)
  WHERE member_owner_id IS NOT NULL AND plan_run_id IS NULL;

CREATE UNIQUE INDEX plan_members_plan_email_uk ON plan_members (plan_id, lower(invite_email))
  WHERE invite_email IS NOT NULL AND member_owner_id IS NULL AND plan_run_id IS NULL;

CREATE INDEX plan_members_run_status_idx ON plan_members (plan_run_id, status)
  WHERE plan_run_id IS NOT NULL;
