-- Track who created / last edited a curated reach.
-- author_id NULL  → owned by H2OFlows (official/system content).
-- author_id NOT NULL → a data_admin created it under their personal account.
ALTER TABLE reaches
  ADD COLUMN author_id       TEXT,
  ADD COLUMN last_edited_by_id TEXT;

CREATE INDEX reaches_author_id_idx ON reaches (author_id) WHERE author_id IS NOT NULL;
