-- Revert: mark all h2oflows user_reaches private again.
-- NOTE: cannot restore exact pre-migration state; assumes none were private before.
UPDATE user_reaches
SET is_private = TRUE
WHERE owner_id = (
    SELECT owner_id FROM user_profiles WHERE LOWER(handle) = 'h2oflows'
    LIMIT 1
);
