-- #314: the data_admin "moderator" tier was removed — a single admin role
-- (site_admin) now gates the whole admin console. Delete the now-dead
-- data_admin grants so they don't linger in the roles UI. site_admin grants
-- (incl. iankco's) are untouched, so no one loses admin access. Idempotent.
DELETE FROM user_roles WHERE role = 'data_admin';
