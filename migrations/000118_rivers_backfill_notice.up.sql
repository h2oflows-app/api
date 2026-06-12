-- No schema changes — this migration just marks that rivers rows with
-- gnis_id set but missing state_abbr/basin should be backfilled via
-- the updated backfill-river-gnis cmd (run once against prod DB).
-- The resolveOrCreateRiver path in handlers/user_reaches.go already
-- backfills incrementally on every new run-add; this covers legacy rows.
-- See: h2oflows-app/api#97
SELECT 1;
