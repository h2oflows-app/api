-- Restore the neutral default 000125 set. No data rewrite here either — the up
-- didn't touch rows, so the down has nothing to put back.
ALTER TABLE user_reaches
  ALTER COLUMN base_color SET DEFAULT 'neutral-3';
