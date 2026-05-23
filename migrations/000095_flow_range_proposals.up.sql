-- Community flow range proposals for public user runs.
-- One proposal per (run, author). Authors can update their own proposal.
-- Votes are tracked separately; vote_count is denormalized for fast sorting.

CREATE TABLE flow_range_proposals (
  id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  user_reach_id  UUID NOT NULL REFERENCES user_reaches(id) ON DELETE CASCADE,
  author_id      TEXT NOT NULL,
  low_max        NUMERIC(8,1),
  running_min    NUMERIC(8,1) NOT NULL,
  running_max    NUMERIC(8,1) NOT NULL,
  high_min       NUMERIC(8,1),
  note           TEXT,
  vote_count     INT NOT NULL DEFAULT 0,
  created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (user_reach_id, author_id)
);

CREATE INDEX flow_range_proposals_reach_votes_idx
  ON flow_range_proposals (user_reach_id, vote_count DESC);

CREATE TABLE flow_range_votes (
  proposal_id    UUID NOT NULL REFERENCES flow_range_proposals(id) ON DELETE CASCADE,
  user_id        TEXT NOT NULL,
  created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (proposal_id, user_id)
);
