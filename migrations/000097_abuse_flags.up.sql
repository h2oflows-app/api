CREATE TABLE abuse_flags (
  id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  target_type TEXT NOT NULL CHECK (target_type IN ('run', 'report')),
  target_id   UUID NOT NULL,
  reporter_id TEXT NOT NULL,
  reason      TEXT NOT NULL DEFAULT 'inappropriate'
                   CHECK (reason IN ('inappropriate','spam','inaccurate','other')),
  note        TEXT,
  resolved_at TIMESTAMPTZ,
  resolved_by TEXT,
  resolution  TEXT CHECK (resolution IN ('dismissed', 'actioned')),
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (target_type, target_id, reporter_id)
);

CREATE INDEX abuse_flags_unresolved_idx ON abuse_flags (created_at DESC)
  WHERE resolved_at IS NULL;
