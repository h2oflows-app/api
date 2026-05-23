-- Privacy flag for user-created runs.
-- FALSE (default) = visible to anyone on the explore map and public search.
-- TRUE            = visible only to the owner.
ALTER TABLE user_reaches
  ADD COLUMN is_private BOOLEAN NOT NULL DEFAULT FALSE;

CREATE INDEX user_reaches_public_idx ON user_reaches (is_private) WHERE is_private = FALSE;
