ALTER TABLE user_reaches
  ALTER COLUMN base_color SET DEFAULT 'red-3';

UPDATE user_reaches
  SET base_color = 'red-3'
  WHERE base_color = 'neutral-3';
