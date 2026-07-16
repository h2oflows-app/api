-- #314: API keys + thin rate-limit state for special-user (and future public
-- API) authoring. One row per owner_id, lazily created on first key issuance
-- or first rate-limited write (see recordRunWrite).
CREATE TABLE user_api_access (
    owner_id            TEXT PRIMARY KEY REFERENCES user_profiles(owner_id) ON DELETE CASCADE,
    api_key_hash         TEXT,
    api_key_last4        TEXT,
    runs_per_hour         INT NOT NULL,
    max_batch             INT NOT NULL,
    requests_per_minute   INT NOT NULL,
    concurrent_jobs       INT NOT NULL,
    usage_hour_start      TIMESTAMPTZ,
    usage_hour_count      INT NOT NULL DEFAULT 0,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
