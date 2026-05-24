CREATE TABLE reach_flow_band_overrides (
  id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id       TEXT NOT NULL,
  reach_id      UUID NOT NULL REFERENCES reaches(id) ON DELETE CASCADE,
  low_max       NUMERIC(8,1),
  running_min   NUMERIC(8,1) NOT NULL,
  running_max   NUMERIC(8,1) NOT NULL,
  high_min      NUMERIC(8,1),
  note          TEXT,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (user_id, reach_id),
  CHECK (running_min < running_max),
  CHECK (low_max  IS NULL OR low_max  <= running_min),
  CHECK (high_min IS NULL OR high_min >= running_max)
);

CREATE INDEX reach_flow_band_overrides_reach_idx ON reach_flow_band_overrides (reach_id);
CREATE INDEX reach_flow_band_overrides_user_idx  ON reach_flow_band_overrides (user_id);
