-- Add 3-tier visibility (private/unlisted/public), tombstone, completeness, adoption.
-- Replaces boolean is_private column. (V1, V2, T1)

CREATE TYPE run_visibility AS ENUM ('private', 'unlisted', 'public');

ALTER TABLE user_reaches
  ADD COLUMN visibility           run_visibility NOT NULL DEFAULT 'private',
  ADD COLUMN published_at         TIMESTAMPTZ,
  ADD COLUMN deleted_at           TIMESTAMPTZ,
  ADD COLUMN completeness_score   REAL NOT NULL DEFAULT 0,
  ADD COLUMN adopted_from_owner_id UUID;

-- Backfill: is_private=false → public (published_at = created_at), true → private.
UPDATE user_reaches
  SET visibility   = 'public',
      published_at = created_at
  WHERE is_private = FALSE;

DROP INDEX IF EXISTS user_reaches_public_idx;
ALTER TABLE user_reaches DROP COLUMN is_private;

-- Index for public-run queries (tombstone excluded).
CREATE INDEX user_reaches_public_idx
  ON user_reaches (visibility)
  WHERE visibility = 'public' AND deleted_at IS NULL;
