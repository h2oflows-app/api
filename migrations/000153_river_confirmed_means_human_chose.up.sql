-- Make river_confirmed mean what its name says, so the pilot can answer
-- "how often does automatic river attribution get it wrong, and where?"
--
-- That question is the documented gate on api#195 (sample the centerline
-- instead of just the put-in). It can only be answered from data collected
-- WHILE people create runs — whether a user corrected an auto-attributed
-- river is not reconstructable afterwards. Hence shipping this before the
-- pilot rather than after.
--
-- What the column meant until now: `river_confirmed = (riverID != nil)` at
-- create time — i.e. "the resolver found a river at all". That is a fact about
-- the SYSTEM, not about a human, and it is true precisely for the runs whose
-- attribution nobody has ever checked. run_upload.go wrote a literal false,
-- forks copied whether the source had a river, and the fork/update paths never
-- touched it again. On prod it reads 3 true / 132 false out of 135 live runs,
-- and none of those 3 involved a person agreeing to anything.
--
-- What it means from here: A HUMAN EXPLICITLY CHOSE THIS RIVER. Set only when
-- an update supplies a river_name that DIFFERS from the stored one — a real
-- correction. Deliberately not set when the submitted name matches, because
-- the run wizard sends river_name on every save and populates it from the NHD
-- suggestion (web RunWizardMap.client.vue), so "name was present" would flag
-- auto-attribution as human-confirmed and reproduce exactly the uselessness
-- this migration is fixing.
--
-- Accepting a correct suggestion therefore does not count as confirmation.
-- That asymmetry is intended: the pilot needs the CORRECTION rate, and a
-- silent accept is indistinguishable from not looking.

-- Every existing row predates the new meaning. The 3 rows currently true were
-- set by the old resolver rule, never by a person, so leaving them would seed
-- the pilot's baseline with false positives.
UPDATE user_reaches SET river_confirmed = false WHERE river_confirmed;

COMMENT ON COLUMN user_reaches.river_confirmed IS
    'TRUE when a human explicitly chose this run''s river by submitting a river_name that differed from the stored one. FALSE means the river came from automatic attribution (put-in snapped to NHD) and nobody has corrected it -- NOT that attribution failed. Accepting a correct auto-suggestion does not set this; only a change does. Read it as the correction rate for api#195.';
