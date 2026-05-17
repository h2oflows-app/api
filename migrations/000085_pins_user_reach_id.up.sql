-- Add user_reach_id FK to rapids and reach_access so KML-imported pins can
-- be owned by a user reach instead of a curated reach. Exactly one parent
-- must be set (XOR constraint).

-- ── rapids ───────────────────────────────────────────────────────────────────

ALTER TABLE rapids
    ALTER COLUMN reach_id DROP NOT NULL,
    ADD COLUMN user_reach_id UUID REFERENCES user_reaches(id) ON DELETE CASCADE;

ALTER TABLE rapids
    ADD CONSTRAINT rapids_parent_xor
    CHECK ((reach_id IS NOT NULL) <> (user_reach_id IS NOT NULL));

-- Existing: UNIQUE (reach_id, name) covers curated side.
-- New partial unique for user-reach side.
CREATE UNIQUE INDEX rapids_user_reach_name_unique
    ON rapids (user_reach_id, name)
    WHERE user_reach_id IS NOT NULL;

CREATE INDEX rapids_user_reach_id_idx
    ON rapids (user_reach_id)
    WHERE user_reach_id IS NOT NULL;

-- ── reach_access ─────────────────────────────────────────────────────────────

ALTER TABLE reach_access
    ALTER COLUMN reach_id DROP NOT NULL,
    ADD COLUMN user_reach_id UUID REFERENCES user_reaches(id) ON DELETE CASCADE;

ALTER TABLE reach_access
    ADD CONSTRAINT reach_access_parent_xor
    CHECK ((reach_id IS NOT NULL) <> (user_reach_id IS NOT NULL));

-- Existing: UNIQUE (reach_id, access_type, name) covers curated side.
-- New partial unique for user-reach side.
CREATE UNIQUE INDEX reach_access_user_reach_type_name_unique
    ON reach_access (user_reach_id, access_type, name)
    WHERE user_reach_id IS NOT NULL;

CREATE INDEX reach_access_user_reach_id_idx
    ON reach_access (user_reach_id)
    WHERE user_reach_id IS NOT NULL;
