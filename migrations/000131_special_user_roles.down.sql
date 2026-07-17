DELETE FROM user_roles
WHERE role = 'h2oflows'
  AND user_id = (SELECT owner_id FROM user_profiles WHERE handle = 'iankco');

-- NOTE: this restores the original CHECK constraint. If any special-user
-- handle roles (or other free-form roles) have been granted since 000131 ran,
-- this ALTER will fail until those rows are removed — down migrations here
-- are only safe immediately after an up, before real special-user role grants
-- accumulate.
ALTER TABLE user_roles ADD CONSTRAINT user_roles_role_check CHECK (role IN ('site_admin', 'data_admin'));
