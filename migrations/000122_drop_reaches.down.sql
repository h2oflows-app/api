-- Irreversible. No down migration.
-- The reaches table and all dropped columns cannot be reconstructed automatically.
-- To recover: restore from a pre-migration database backup.
SELECT 'irreversible — restore from backup';
