-- Staging-only recovery: 117 was applied with the old user_basin_overrides schema.
-- Drop the old table and create the correct per-river table.
-- On prod 117 already creates user_river_basin_overrides so CREATE IF NOT EXISTS is a no-op.
DROP TABLE IF EXISTS user_basin_overrides;

CREATE TABLE IF NOT EXISTS user_river_basin_overrides (
    id         UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    TEXT        NOT NULL,
    river_id   UUID        NOT NULL REFERENCES rivers(id) ON DELETE CASCADE,
    basin_key  TEXT        NOT NULL CHECK (char_length(basin_key) BETWEEN 1 AND 80),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (user_id, river_id)
);

CREATE INDEX IF NOT EXISTS ON user_river_basin_overrides (user_id);
