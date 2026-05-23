-- Fork lineage: track where a user_reach was cloned from.
-- At most one source column is set (CHECK enforces mutual exclusivity).
ALTER TABLE user_reaches
  ADD COLUMN forked_from_reach_id      UUID REFERENCES reaches(id)      ON DELETE SET NULL,
  ADD COLUMN forked_from_user_reach_id UUID REFERENCES user_reaches(id) ON DELETE SET NULL;

ALTER TABLE user_reaches
  ADD CONSTRAINT user_reaches_fork_single_source CHECK (
    forked_from_reach_id IS NULL OR forked_from_user_reach_id IS NULL
  );
