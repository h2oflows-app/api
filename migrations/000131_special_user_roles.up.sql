-- #314: role becomes free-form TEXT — a role name is either a legacy system
-- role (site_admin, data_admin) or a special user's handle (validated in Go,
-- see AssignRole / isValidRoleName). Drop the old CHECK so handle-as-role
-- grants can be inserted.
ALTER TABLE user_roles DROP CONSTRAINT IF EXISTS user_roles_role_check;

-- Seed iankco with the h2oflows role in the SAME deploy that removes the
-- implicit data_admin/site_admin fallback for editing h2oflows-owned runs —
-- no lockout window. ON CONFLICT DO NOTHING is safe: the partial unique index
-- user_roles_global_uniq (user_id, role) WHERE river_id IS NULL covers this
-- global (non-river-scoped) grant.
INSERT INTO user_roles (user_id, role)
SELECT owner_id, 'h2oflows' FROM user_profiles WHERE handle = 'iankco'
ON CONFLICT DO NOTHING;
