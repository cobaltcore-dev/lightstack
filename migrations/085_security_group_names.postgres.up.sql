-- SPEC-033 round 2 (FR-026, research R53): group names may repeat within a project, as
-- upstream's do; only `default` is reserved, one per project.
--
-- 001 created UNIQUE (project_id, name) without a name, so PostgreSQL named it. Look it up
-- rather than assume the name, and stop if it is not there: a database that lost it some
-- other way is not one this migration understands.
DO $$
DECLARE
    unique_name text;
BEGIN
    SELECT conname INTO unique_name FROM pg_constraint
     WHERE conrelid = 'security_groups'::regclass AND contype = 'u'
       AND pg_get_constraintdef(oid) = 'UNIQUE (project_id, name)';
    IF unique_name IS NULL THEN
        RAISE EXCEPTION 'security_groups has no UNIQUE (project_id, name) constraint to drop';
    END IF;
    EXECUTE format('ALTER TABLE security_groups DROP CONSTRAINT %I', unique_name);
END $$;

CREATE UNIQUE INDEX security_groups_one_default ON security_groups (project_id) WHERE name = 'default';

-- The constraint's index was the only one led by project_id: a project's groups by name, and its
-- listings, read through this one now.
CREATE INDEX idx_security_groups_project_name ON security_groups (project_id, name);
