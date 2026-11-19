-- Rollback groups tables
--
-- Drops only what 028 created. groups and group_members are 010's: 028 creates them only IF
-- NOT EXISTS, so on a database migrated in order it creates nothing but these two indexes, and
-- dropping the tables here failed on group_users (019), which references groups.
DROP INDEX IF EXISTS idx_group_members_user;
DROP INDEX IF EXISTS idx_groups_domain;
