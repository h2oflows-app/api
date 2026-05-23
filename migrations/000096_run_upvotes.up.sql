-- Per-run upvotes. One per (run, user). No downvotes.
CREATE TABLE run_upvotes (
  user_reach_id  UUID NOT NULL REFERENCES user_reaches(id) ON DELETE CASCADE,
  user_id        TEXT NOT NULL,
  created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (user_reach_id, user_id)
);

CREATE INDEX run_upvotes_reach_idx ON run_upvotes (user_reach_id);
