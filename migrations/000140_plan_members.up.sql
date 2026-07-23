-- #246: Trip Calendar — plan_members, unified invites + crew requests
-- (one table, `origin` discriminator: invite vs join-request).

CREATE TABLE plan_members (
  id                 UUID                PRIMARY KEY DEFAULT gen_random_uuid(),
  plan_id            UUID                NOT NULL REFERENCES plans(id) ON DELETE CASCADE,
  member_owner_id    TEXT                REFERENCES user_profiles(owner_id) ON DELETE CASCADE, -- NULL until email invite resolves
  invite_email       TEXT, -- lowercased
  invite_handle      TEXT, -- snapshot
  invited_by         TEXT,
  origin             plan_member_origin  NOT NULL,
  status             plan_member_status  NOT NULL,
  plan_run_id        UUID                REFERENCES plan_runs(id) ON DELETE CASCADE, -- optional
  invite_token_hash  TEXT, -- SHA-256, reuse auth.APIKey hash shape
  message            TEXT,
  dismissed_at       TIMESTAMPTZ, -- dismiss keeps row in feed
  responded_at       TIMESTAMPTZ,
  created_at         TIMESTAMPTZ         NOT NULL DEFAULT NOW(),
  updated_at         TIMESTAMPTZ         NOT NULL DEFAULT NOW(),
  CHECK (member_owner_id IS NOT NULL OR invite_email IS NOT NULL)
);

CREATE UNIQUE INDEX plan_members_owner_uk ON plan_members (plan_id, member_owner_id)
  WHERE member_owner_id IS NOT NULL;

CREATE UNIQUE INDEX plan_members_email_uk ON plan_members (plan_id, lower(invite_email))
  WHERE invite_email IS NOT NULL AND member_owner_id IS NULL;

CREATE INDEX plan_members_feed_idx ON plan_members (member_owner_id, status);
CREATE INDEX plan_members_plan_idx ON plan_members (plan_id, status);

CREATE INDEX plan_members_token_idx ON plan_members (invite_token_hash)
  WHERE invite_token_hash IS NOT NULL;
