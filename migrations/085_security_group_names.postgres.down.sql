-- Fails while two groups of one project share a name: those must be renamed first.
DROP INDEX idx_security_groups_project_name;
DROP INDEX security_groups_one_default;
ALTER TABLE security_groups ADD CONSTRAINT security_groups_project_id_name_key UNIQUE (project_id, name);
