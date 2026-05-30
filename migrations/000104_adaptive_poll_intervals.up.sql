ALTER TABLE gauges
    ADD COLUMN poll_interval_seconds     INTEGER
        CHECK (poll_interval_seconds BETWEEN 300 AND 86400),
    ADD COLUMN detected_interval_seconds INTEGER
        CHECK (detected_interval_seconds > 0);

COMMENT ON COLUMN gauges.poll_interval_seconds     IS 'Manual override: minimum seconds between polls. NULL = use detected or source default.';
COMMENT ON COLUMN gauges.detected_interval_seconds IS 'Auto-derived from successive reading timestamps (N>=5). NULL until enough samples exist.';
