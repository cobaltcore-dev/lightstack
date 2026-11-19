-- groups.domain_id becomes what users.domain_id and projects.domain_id already are: a UUID
-- referencing domains(id).
--
-- Migration 010 created it as VARCHAR(255) DEFAULT 'default' with no foreign key. The UUID
-- definitions in 019 and 028 never applied: both are CREATE TABLE IF NOT EXISTS against the
-- table 010 had already created. And until SPEC-022 the group handler stored whatever
-- domain_id it was given — the `default` alias, or a string naming no domain at all — so one
-- domain could hold the same group name twice, once under the alias and once under its id.
--
-- Existing rows are normalised first, and only where the answer is not a guess:
--   * 'default', '' and NULL meant the Default domain (the alias, and an omitted reference)
--     and become its id; an id in another letter case becomes the canonical one;
--   * a value that names no existing domain stops the migration, naming the rows, rather
--     than moving a group into a domain nobody chose;
--   * a name that would then appear twice in one domain stops it too, rather than deleting
--     or renaming someone's group.
-- A stopped migration changes nothing (the file runs as one transaction) but leaves version
-- 72 marked dirty: fix the named rows, set schema_migrations back to version 71 with
-- dirty = false, and restart.

DO $$
DECLARE
    default_id uuid;
    offenders  text;
BEGIN
    SELECT id INTO default_id FROM domains WHERE name = 'Default';
    IF default_id IS NULL THEN
        RAISE EXCEPTION 'migration 072: no domain is named Default, so the default alias cannot be resolved';
    END IF;

    SELECT string_agg(name, ', ' ORDER BY name) INTO offenders
      FROM (SELECT name
              FROM groups
             GROUP BY name,
                      CASE WHEN domain_id IS NULL OR domain_id IN ('default', '') THEN default_id::text
                           ELSE lower(domain_id) END
            HAVING count(*) > 1) twice;
    IF offenders IS NOT NULL THEN
        RAISE EXCEPTION 'migration 072: these group names would appear twice in one domain: %', offenders;
    END IF;

    UPDATE groups SET domain_id = default_id::text
     WHERE domain_id IS NULL OR domain_id IN ('default', '');

    SELECT string_agg(format('%s (%s)', g.name, g.domain_id), ', ' ORDER BY g.name) INTO offenders
      FROM groups g
     WHERE NOT EXISTS (SELECT 1 FROM domains d WHERE d.id::text = lower(g.domain_id));
    IF offenders IS NOT NULL THEN
        RAISE EXCEPTION 'migration 072: these groups name no existing domain: %', offenders;
    END IF;
END $$;

ALTER TABLE groups ALTER COLUMN domain_id DROP DEFAULT;
ALTER TABLE groups ALTER COLUMN domain_id TYPE UUID USING domain_id::uuid;
ALTER TABLE groups ALTER COLUMN domain_id SET NOT NULL;
ALTER TABLE groups ADD CONSTRAINT groups_domain_id_fkey
    FOREIGN KEY (domain_id) REFERENCES domains(id) ON DELETE CASCADE;
