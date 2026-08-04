-- Make river_sequence total per river, so ordering has ONE transitive key
-- (web#406).
--
-- mig 000151 left river_sequence NULL for any run whose up_comid is not in
-- its river's flowline order: a fork (no comid at all), a run on a tributary
-- filed under the parent river, or a put-in that snapped to the far side of a
-- confluence. Every list of runs therefore had to blend two keys, and the two
-- implementations disagreed:
--
--   server  ORDER BY river_sequence ASC NULLS LAST, put_in_elevation_ft DESC
--   client  compare by sequence only when BOTH sides have one, else elevation
--
-- The server demoted every unsequenced run to the bottom of its river; the
-- client placed it by elevation among its neighbours. The client's rule reads
-- better but is NOT TRANSITIVE, which is the real defect:
--
--   A(seq=1, 50ft)  B(no seq, 100ft)  C(seq=2, 150ft)
--   A < C by sequence;  B < A by elevation;  C < B by elevation  →  C < B < A < C
--
-- Array.prototype.sort does not reject a cyclic comparator, it just returns an
-- implementation-defined order. That needs elevation to CONTRADICT topology to
-- bite, which is precisely the flat-river case topology exists for — so it is
-- latent exactly where the feature is supposed to earn its keep.
--
-- A pairwise comparator can only be transitive if each tier is a total
-- preorder, i.e. NULLS LAST. Correctness in the mixed case therefore has to
-- come from the DATA, not the comparator: give every orderable run a sequence
-- so the NULL branch stops being reachable in practice.
--
-- An inferred sequence is derived from where the run's put-in elevation falls
-- among the SURVEYED runs on the same river, so elevation stays the honest
-- documented fallback — applied once at write time instead of re-derived at
-- every comparison.
--
-- This flag keeps the two provenances distinct. It matters because a surveyed
-- river_sequence is a real index into river_flowline_order and an inferred one
-- is not: joining on it would silently match the wrong flowline. It also lets
-- a re-sync recompute guesses without touching measured topology, and keeps
-- "how well is this run attributed" answerable in the data — the same question
-- api#195 is deferred pending pilot feedback on.
ALTER TABLE user_reaches
    ADD COLUMN IF NOT EXISTS river_sequence_inferred BOOLEAN NOT NULL DEFAULT FALSE;

COMMENT ON COLUMN user_reaches.river_sequence_inferred IS
    'TRUE when river_sequence was interpolated from put_in_elevation_ft against the surveyed runs on this river, rather than read from river_flowline_order. An inferred value orders correctly but is NOT a valid comid index -- never join it back to river_flowline_order.';

-- Deliberately NOT added to gauges. An unsequenced gauge is unsequenced
-- precisely because its comid is in no river's flowline order, which means we
-- do not know which river it is on -- and interpolation needs a river to
-- interpolate within. Gauges standing in for a run already sort by that run's
-- keys instead (api#192, web#407), so they inherit this fix; a standalone
-- gauge off every known mainstem stays genuinely unordered. DWR gauges also
-- carry no elevation at all (0 of 19 in prod), so there would be nothing to
-- interpolate from even if the river were known.
