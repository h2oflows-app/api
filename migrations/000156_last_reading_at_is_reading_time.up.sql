-- Repoint gauges.last_reading_at at the reading's own timestamp (api#206).
--
-- The column was written as NOW() on every successful poll, which made it a
-- byte-identical duplicate of last_poll_success_at and meant "when we last
-- fetched", not "when the data was measured". Measured on prod before this
-- change, it ran 18-63 minutes ahead of the newest row in gauge_readings for
-- the same gauge, so every surface that showed it overstated freshness.
--
-- The code fix (recordSuccess now stamps the source's timestamp) uses
-- GREATEST(last_reading_at, $2) so a source re-serving an older value cannot
-- rewind it. That guard would otherwise PIN every existing row at its inflated
-- poll time until real readings caught up, so the historical values have to be
-- corrected here rather than left to heal.
UPDATE gauges g
SET last_reading_at = latest.ts
FROM (
    SELECT gauge_id, MAX(timestamp) AS ts
    FROM gauge_readings
    GROUP BY gauge_id
) latest
WHERE latest.gauge_id = g.id
  AND (g.last_reading_at IS NULL OR g.last_reading_at <> latest.ts);

-- Gauges with no rows in gauge_readings are deliberately left alone. Readings
-- are retained for 30 days, so an empty set means either a brand-new gauge that
-- has not polled yet or one that has published nothing in a month. Neither has
-- a true reading time available to write, and nulling the column would throw
-- away the only (wrong, but bounded) information we have. They self-correct on
-- their next successful poll.

COMMENT ON COLUMN gauges.last_reading_at IS
  'Timestamp the SOURCE put on the most recent reading. Not when we polled — that is last_poll_success_at. Written by poller.recordSuccess as GREATEST(existing, reading.Timestamp).';
