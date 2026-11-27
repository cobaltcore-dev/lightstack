-- lightstack:foreign-keys-off
-- Rebuilds security_groups with UNIQUE (project_id, name) again, as the up file rebuilt it
-- without. Fails while two groups of one project share a name: those must be renamed first.
DROP INDEX idx_security_groups_project_name;
DROP INDEX security_groups_one_default;
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
    UNIQUE (project_id, name),
    FOREIGN KEY (project_id) REFERENCES projects(id) ON DELETE CASCADE
) STRICT;
INSERT INTO security_groups_rebuilt (id, name, project_id, description, created_at, updated_at, revision_number, stateful)
SELECT id, name, project_id, description, created_at, updated_at, revision_number, stateful FROM security_groups;
DROP TABLE security_groups;
ALTER TABLE security_groups_rebuilt RENAME TO security_groups;
