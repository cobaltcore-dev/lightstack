-- SQLite stores all resource identifiers as TEXT. Keep the same relational
-- contract as the PostgreSQL variant without UUID column types (SPEC-028).
CREATE TABLE IF NOT EXISTS application_credential_roles (
    application_credential_id TEXT NOT NULL REFERENCES application_credentials(id) ON DELETE CASCADE,
    role_id TEXT NOT NULL REFERENCES roles(id) ON DELETE CASCADE,
    PRIMARY KEY (application_credential_id, role_id)
) STRICT;

CREATE INDEX IF NOT EXISTS idx_application_credential_roles_role
    ON application_credential_roles(role_id);
