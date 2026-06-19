-- Change base_color default from 'red-3' to 'neutral-3' on user_reaches.
-- Existing rows that still have the old default are reset to neutral so that
-- runs with no configured flow bands don't show a red chart background.
ALTER TABLE user_reaches
  ALTER COLUMN base_color SET DEFAULT 'neutral-3';

UPDATE user_reaches
  SET base_color = 'neutral-3'
  WHERE base_color = 'red-3';
