-- Reverse 000112: restore is_private bool, drop new columns and enum.

ALTER TABLE user_reaches
  ADD COLUMN is_private BOOLEAN NOT NULL DEFAULT FALSE;

-- private/unlisted → is_private=true; public → is_private=false (default).
UPDATE user_reaches
  SET is_private = TRUE
  WHERE visibility IN ('private', 'unlisted');

DROP INDEX IF EXISTS user_reaches_public_idx;

CREATE INDEX user_reaches_public_idx
  ON user_reaches (is_private)
  WHERE is_private = FALSE;

ALTER TABLE user_reaches
  DROP COLUMN visibility,
  DROP COLUMN published_at,
  DROP COLUMN deleted_at,
  DROP COLUMN completeness_score,
  DROP COLUMN adopted_from_owner_id;

DROP TYPE IF EXISTS run_visibility;
