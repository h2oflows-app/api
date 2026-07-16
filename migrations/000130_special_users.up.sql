-- Special users (#314): opt-in "official" accounts (h2oflows curator + future
-- partner orgs) that can be granted authoring rights via user_roles and have
-- their own anon-visibility toggle. is_special replaces the hardcoded sentinel
-- UUID check that used to mean "official"; public_on_map gates whether an
-- anonymous (unauthenticated) caller sees this account's runs on map/list
-- endpoints.
-- delete_locked: UI/API safety latch against inadvertently deleting a special
-- account anchoring a large run database (h2oflows, AW, ...). h2oflows is
-- permanently locked (the API refuses to unlock it); other special users can
-- be unlocked by an admin before a deliberate delete.
ALTER TABLE user_profiles
    ADD COLUMN display_name TEXT,
    ADD COLUMN is_special BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN public_on_map BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN delete_locked BOOLEAN NOT NULL DEFAULT false;

-- The h2oflows sentinel (mig 000105) becomes the first special user, visible
-- to anonymous callers immediately — no behavior change for existing traffic.
-- Permanently delete-locked.
UPDATE user_profiles
SET is_special = true, public_on_map = true, display_name = 'H2OFlows', delete_locked = true
WHERE owner_id = '00000000-0000-0000-0000-000000000001';
