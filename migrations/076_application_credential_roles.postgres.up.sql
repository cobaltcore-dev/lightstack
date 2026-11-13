-- application_credential_roles — the roles an application credential narrows a token to.
--
-- The handlers in internal/keystone/application_credentials.go have referenced this
-- table since migration 029 introduced application credentials, but 029 never created
-- it: every roles lookup failed with `relation "application_credential_roles" does not
-- exist`, the handlers swallowed the error, and the `roles` key was omitted from list
-- and show responses — upstream always includes it, and the pinned python-openstackclient
-- crashes rendering a credential without one (SPEC-028).
--
-- Fresh installs get the table; installs migrated past 029 get it too, empty, which is
-- exactly what their credentials answer today (no roles were ever persisted).
CREATE TABLE IF NOT EXISTS application_credential_roles (
    application_credential_id UUID NOT NULL REFERENCES application_credentials(id) ON DELETE CASCADE,
    role_id UUID NOT NULL REFERENCES roles(id) ON DELETE CASCADE,
    PRIMARY KEY (application_credential_id, role_id)
);

CREATE INDEX IF NOT EXISTS idx_application_credential_roles_role ON application_credential_roles(role_id);
