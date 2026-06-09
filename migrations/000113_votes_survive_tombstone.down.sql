-- Reverse 000113: restore CASCADE so votes deleted with run on hard-delete.

ALTER TABLE run_upvotes
  DROP CONSTRAINT run_upvotes_user_reach_id_fkey;

ALTER TABLE run_upvotes
  ADD CONSTRAINT run_upvotes_user_reach_id_fkey
    FOREIGN KEY (user_reach_id) REFERENCES user_reaches(id) ON DELETE CASCADE;
