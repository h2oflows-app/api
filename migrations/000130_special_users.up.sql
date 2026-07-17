-- Special users (#314): opt-in "official" accounts (h2oflows curator + future
-- partner orgs) that can be granted authoring rights via user_roles and have
-- their own anon-visibility toggle. is_special replaces the hardcoded sentinel
-- UUID check that used to mean "official"; public_on_map gates whether an
-- anonymous (unauthenticated) caller sees this account's runs on map/list
-- endpoints.
ALTER TABLE user_profiles
    ADD COLUMN display_name TEXT,
    ADD COLUMN is_special BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN public_on_map BOOLEAN NOT NULL DEFAULT false;

-- The h2oflows sentinel (mig 000105) becomes the first special user, visible
-- to anonymous callers immediately — no behavior change for existing traffic.
UPDATE user_profiles
SET is_special = true, public_on_map = true, display_name = 'H2OFlows'
WHERE owner_id = '00000000-0000-0000-0000-000000000001';
