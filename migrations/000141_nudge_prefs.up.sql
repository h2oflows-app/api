-- #246: Trip Calendar — nudge dismissals + per-user calendar prefs.
-- Tier-A arm 2 (run_interactions) is DESCOPED from MVP; not created here.

CREATE TABLE nudge_dismissals (
  owner_id       TEXT        NOT NULL,
  user_reach_id  UUID        NOT NULL REFERENCES user_reaches(id) ON DELETE CASCADE,
  nudge_date     DATE        NOT NULL,
  created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  PRIMARY KEY (owner_id, user_reach_id, nudge_date)
);

CREATE TABLE user_calendar_prefs (
  owner_id                TEXT        PRIMARY KEY,
  season_goal_days        INT,
  tz                      TEXT,
  last_nudge_prompt_date  DATE,
  updated_at              TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
