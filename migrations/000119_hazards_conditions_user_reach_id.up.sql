-- Runs-unification Phase 1: add user_reach_id FK to hazards and reach_conditions
-- so user-authored runs can carry hazard and condition reports, mirroring the
-- dual-parent (XOR) pattern already applied to rapids/reach_access (mig 085).
-- Exactly one parent must be set.

-- ── hazards ──────────────────────────────────────────────────────────────────

ALTER TABLE hazards
    ALTER COLUMN reach_id DROP NOT NULL,
    ADD COLUMN user_reach_id UUID REFERENCES user_reaches(id) ON DELETE CASCADE;

ALTER TABLE hazards
    ADD CONSTRAINT hazards_parent_xor
    CHECK ((reach_id IS NOT NULL) <> (user_reach_id IS NOT NULL));

CREATE INDEX hazards_user_reach_id_idx
    ON hazards (user_reach_id)
    WHERE user_reach_id IS NOT NULL;

CREATE INDEX hazards_user_reach_active_idx
    ON hazards (user_reach_id)
    WHERE user_reach_id IS NOT NULL AND active = TRUE;

-- ── reach_conditions ─────────────────────────────────────────────────────────

ALTER TABLE reach_conditions
    ALTER COLUMN reach_id DROP NOT NULL,
    ADD COLUMN user_reach_id UUID REFERENCES user_reaches(id) ON DELETE CASCADE;

ALTER TABLE reach_conditions
    ADD CONSTRAINT reach_conditions_parent_xor
    CHECK ((reach_id IS NOT NULL) <> (user_reach_id IS NOT NULL));

CREATE INDEX reach_conditions_user_reach_id_idx
    ON reach_conditions (user_reach_id)
    WHERE user_reach_id IS NOT NULL;

CREATE INDEX reach_conditions_user_reach_expiry_idx
    ON reach_conditions (user_reach_id, expires_at DESC, created_at DESC)
    WHERE user_reach_id IS NOT NULL;
