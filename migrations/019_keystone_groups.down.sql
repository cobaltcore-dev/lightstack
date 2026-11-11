-- Drop Keystone groups tables
--
-- Drops only what 019 created. groups and idx_groups_domain_id are 010's: 019 creates them
-- only IF NOT EXISTS, so on a database migrated in order it creates group_users and its two
-- indexes alone, and dropping groups here failed on group_members (010), which references it.
DROP INDEX IF EXISTS idx_group_users_user_id;
DROP INDEX IF EXISTS idx_group_users_group_id;
DROP TABLE IF EXISTS group_users;
