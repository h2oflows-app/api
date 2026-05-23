DROP INDEX IF EXISTS reaches_author_id_idx;
ALTER TABLE reaches
  DROP COLUMN IF EXISTS author_id,
  DROP COLUMN IF EXISTS last_edited_by_id;
