-- #314: delete_locked — UI/API safety latch against inadvertently deleting a
-- special account anchoring a large run database (h2oflows, AW, ...).
-- h2oflows is permanently locked (the API refuses to unlock it, and
-- DeleteSpecialUser refuses the anchor account outright regardless of this
-- flag); other special users are created locked and can be unlocked by an
-- admin before a deliberate delete.
--
-- Own migration (not folded into 000130) because staging already applied
-- 000130 in its original form — golang-migrate never re-runs applied versions.
ALTER TABLE user_profiles
    ADD COLUMN IF NOT EXISTS delete_locked BOOLEAN NOT NULL DEFAULT false;

-- Existing special users get the safe default retroactively; h2oflows is
-- (re-)asserted locked. Idempotent.
UPDATE user_profiles SET delete_locked = true WHERE is_special;
