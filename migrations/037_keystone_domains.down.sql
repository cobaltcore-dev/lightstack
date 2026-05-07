-- Rollback domains
-- Drop FK constraints referencing domains before dropping the table
ALTER TABLE IF EXISTS users DROP CONSTRAINT IF EXISTS users_domain_id_fkey;
ALTER TABLE IF EXISTS projects DROP CONSTRAINT IF EXISTS projects_domain_id_fkey;
DROP TABLE IF EXISTS domains;
