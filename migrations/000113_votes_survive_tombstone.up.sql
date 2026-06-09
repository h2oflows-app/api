-- run_upvotes: change FK to RESTRICT so votes survive soft-delete (tombstone).
-- GC (hard-delete of user_reaches row) must explicitly delete votes first.
-- Votes frozen on tombstone; gone only when GC hard-deletes the run. (V12, V13)

ALTER TABLE run_upvotes
  DROP CONSTRAINT run_upvotes_user_reach_id_fkey;

ALTER TABLE run_upvotes
  ADD CONSTRAINT run_upvotes_user_reach_id_fkey
    FOREIGN KEY (user_reach_id) REFERENCES user_reaches(id) ON DELETE RESTRICT;
