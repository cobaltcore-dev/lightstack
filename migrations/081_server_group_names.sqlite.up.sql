-- Remove the uniqueness rule from pre-release SPEC-028 installations.
DROP INDEX IF EXISTS uq_server_groups_project_name;
