-- Snapshot fork attribution at fork time so it survives source deletion (V2).
ALTER TABLE user_reaches
  ADD COLUMN original_author_handle    TEXT,
  ADD COLUMN original_author_owner_id  UUID,
  ADD COLUMN original_forked_at        TIMESTAMPTZ,
  ADD COLUMN last_modified_after_fork_at TIMESTAMPTZ;
