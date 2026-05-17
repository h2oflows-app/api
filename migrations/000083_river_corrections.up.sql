CREATE TABLE river_corrections (
  id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  river_id        UUID        NOT NULL REFERENCES rivers(id) ON DELETE CASCADE,
  proposed_by     TEXT        NOT NULL,
  field           TEXT        NOT NULL CHECK (field IN ('basin', 'state_abbr')),
  proposed_value  TEXT        NOT NULL,
  note            TEXT,
  status          TEXT        NOT NULL DEFAULT 'pending'
                  CHECK (status IN ('pending', 'accepted', 'rejected')),
  reviewed_by     TEXT,
  reviewed_at     TIMESTAMPTZ,
  review_note     TEXT,
  created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX river_corrections_status_idx ON river_corrections (status) WHERE status = 'pending';
CREATE INDEX river_corrections_river_idx  ON river_corrections (river_id);
