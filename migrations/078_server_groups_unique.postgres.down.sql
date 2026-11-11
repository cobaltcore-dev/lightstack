-- Reverse of 078: drop the server-group name uniqueness constraint. The
-- folded duplicates are not resurrected (they were indistinguishable anyway).
ALTER TABLE server_groups
    DROP CONSTRAINT IF EXISTS uq_server_groups_project_name;
