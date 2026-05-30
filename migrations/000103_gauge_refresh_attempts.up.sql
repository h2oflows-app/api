CREATE TABLE gauge_refresh_attempts (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      UUID        NOT NULL,
    gauge_id     UUID        NOT NULL REFERENCES gauges(id) ON DELETE CASCADE,
    attempted_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX gauge_refresh_attempts_lookup_idx
    ON gauge_refresh_attempts (user_id, gauge_id, attempted_at);
