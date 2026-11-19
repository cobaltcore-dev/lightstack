-- Reverse SPEC-014 migration 062.
DELETE FROM services WHERE id = '00000000-0000-0000-0000-000000000016'; -- cascades to its endpoint
DROP INDEX IF EXISTS idx_credentials_access_key;
ALTER TABLE credentials DROP COLUMN IF EXISTS access_key;
