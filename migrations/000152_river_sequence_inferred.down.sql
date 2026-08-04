-- Runs whose sequence was interpolated must lose it, not keep it: without the
-- flag there is no way to tell a guessed sequence from a surveyed one, and a
-- guessed value looks like a valid river_flowline_order index to every reader.
UPDATE user_reaches SET river_sequence = NULL WHERE river_sequence_inferred;

ALTER TABLE user_reaches DROP COLUMN IF EXISTS river_sequence_inferred;
