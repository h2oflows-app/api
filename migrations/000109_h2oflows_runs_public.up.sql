-- Set all h2oflows-owned user_reaches to is_private=false (V18).
-- h2oflows runs are official curator content and must be publicly visible.
UPDATE user_reaches
SET is_private = FALSE
WHERE owner_id = (
    SELECT owner_id FROM user_profiles WHERE LOWER(handle) = 'h2oflows'
    LIMIT 1
);
