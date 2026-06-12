-- Per-river basin override. A user can reassign any river to a different
-- basin grouping label without touching the shared rivers table.
-- basin_key is the free-text label that replaces rivers.basin for this user.
CREATE TABLE IF NOT EXISTS user_river_basin_overrides (
    id         UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    TEXT        NOT NULL,
    river_id   UUID        NOT NULL REFERENCES rivers(id) ON DELETE CASCADE,
    basin_key  TEXT        NOT NULL CHECK (char_length(basin_key) BETWEEN 1 AND 80),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (user_id, river_id)
);

CREATE INDEX ON user_river_basin_overrides (user_id);
