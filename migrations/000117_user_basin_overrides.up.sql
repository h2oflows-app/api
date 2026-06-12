-- User basin display-name overrides.
-- basin_key = raw rivers.basin (HUC name) — one override re-labels every run in that basin.
-- state_abbr is canonical and not overridable, but is part of the key so the
-- same basin name in different states can have different display labels.
CREATE TABLE IF NOT EXISTS user_basin_overrides (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      TEXT        NOT NULL,
    state_abbr   TEXT        NOT NULL,
    basin_key    TEXT        NOT NULL,
    display_name TEXT        NOT NULL CHECK (char_length(display_name) BETWEEN 1 AND 80),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (user_id, state_abbr, basin_key)
);

CREATE INDEX ON user_basin_overrides (user_id);
