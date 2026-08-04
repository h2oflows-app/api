-- Topology-based upstream→downstream ordering (web#392).
--
-- Ordering has so far leaned on put_in_elevation_ft DESC (mig 000150) with
-- longitude as a fallback. Both are PROXIES for flow direction and each fails
-- predictably: elevation on flat rivers (the Green through Canyonlands, the
-- Menominee) where the real drop is inside USGS EPQS noise, and longitude on
-- any river not flowing west→east.
--
-- NHDPlus topology is not a proxy. NLDI's DM (downstream mainstem) navigation
-- returns flowlines in true downstream order, so indexing a run's up_comid --
-- or a gauge's comid -- into that order gives an exact sequence that is
-- direction-agnostic and gradient-independent.

-- Cached NLDI navigation result, one row per flowline per river. Keeping it
-- means a newly-added run or gauge gets its sequence from a lookup instead of
-- another NLDI round trip; only a river's first sequencing costs network.
CREATE TABLE IF NOT EXISTS river_flowline_order (
    river_id UUID    NOT NULL REFERENCES rivers(id) ON DELETE CASCADE,
    comid    TEXT    NOT NULL,
    seq      INTEGER NOT NULL,
    PRIMARY KEY (river_id, comid)
);

-- Lookup path is (river_id, comid) via the PK for assignment, and comid alone
-- when resolving which rivers a flowline participates in.
CREATE INDEX IF NOT EXISTS idx_river_flowline_order_comid
    ON river_flowline_order (comid);

-- Denormalized sort key. Deliberately a column on the row rather than a join:
-- ~10 read paths feed sorted lists, Go does not validate embedded SQL, and a
-- placeholder/scan mismatch surfaces as a runtime 500. Adding one nullable
-- column to existing SELECTs is how put_in_elevation_ft shipped safely.
-- NULL = this river has not been sequenced (or the row sits on a tributary
-- the mainstem navigation does not cover), and callers fall back to elevation.
ALTER TABLE user_reaches ADD COLUMN IF NOT EXISTS river_sequence INTEGER;
ALTER TABLE gauges       ADD COLUMN IF NOT EXISTS river_sequence INTEGER;

-- Staleness marker so a re-sync can skip rivers already resolved, and so a
-- backfill can be resumed rather than restarted.
ALTER TABLE rivers ADD COLUMN IF NOT EXISTS topology_synced_at TIMESTAMPTZ;
