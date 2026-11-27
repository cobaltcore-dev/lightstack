-- lightstack:foreign-keys-off
-- See 085_security_group_names.postgres.up.sql. SQLite cannot drop a table constraint, so the
-- table is rebuilt: new table, copy, drop, rename. security_group_rules and port_security_groups
-- reference it with ON DELETE CASCADE, so the DROP would delete their rows with foreign keys on.
-- The marker above has the migrator turn them off for this file, run it in a transaction of its
-- own, and refuse it unless PRAGMA foreign_key_check finds nothing (migrations/README.md).
CREATE TABLE "security_groups_rebuilt" (
    "id" TEXT NOT NULL DEFAULT (gen_random_uuid()),
    "name" TEXT NOT NULL CHECK (length("name") <= 255),
    "project_id" TEXT,
    "description" TEXT,
    "created_at" TEXT DEFAULT (now()),
    "updated_at" TEXT DEFAULT (now()),
    "revision_number" INTEGER NOT NULL DEFAULT 0,
    "stateful" INTEGER NOT NULL DEFAULT 1,
    PRIMARY KEY ("id"),
    FOREIGN KEY (project_id) REFERENCES projects(id) ON DELETE CASCADE
) STRICT;
INSERT INTO security_groups_rebuilt (id, name, project_id, description, created_at, updated_at, revision_number, stateful)
SELECT id, name, project_id, description, created_at, updated_at, revision_number, stateful FROM security_groups;
DROP TABLE security_groups;
ALTER TABLE security_groups_rebuilt RENAME TO security_groups;

CREATE UNIQUE INDEX security_groups_one_default ON security_groups (project_id) WHERE name = 'default';
CREATE INDEX idx_security_groups_project_name ON security_groups (project_id, name);
