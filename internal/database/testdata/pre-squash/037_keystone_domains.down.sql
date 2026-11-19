-- Rollback domains
--
-- Drops only what 037 created. The domains table and its foreign keys are 008's and the
-- 'Default' row is 009's: 037 creates the table only IF NOT EXISTS and seeds ON CONFLICT DO
-- NOTHING, so on a database migrated in order it creates nothing but these two indexes, and
-- dropping the table here broke 009's down, which runs later.
DROP INDEX IF EXISTS idx_domains_enabled;
DROP INDEX IF EXISTS idx_domains_name;
