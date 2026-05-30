ALTER TABLE gauges
    DROP COLUMN IF EXISTS poll_interval_seconds,
    DROP COLUMN IF EXISTS detected_interval_seconds;
