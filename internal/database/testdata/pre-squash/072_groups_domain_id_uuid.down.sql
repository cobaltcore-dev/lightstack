-- Restores the column migration 010 created. The alias normalisation is not undone: the
-- Default domain's id is as valid in the VARCHAR column as the alias was.
ALTER TABLE groups DROP CONSTRAINT IF EXISTS groups_domain_id_fkey;
ALTER TABLE groups ALTER COLUMN domain_id DROP NOT NULL;
ALTER TABLE groups ALTER COLUMN domain_id TYPE VARCHAR(255) USING domain_id::text;
ALTER TABLE groups ALTER COLUMN domain_id SET DEFAULT 'default';
