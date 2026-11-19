-- Remove the uniqueness index from the short-lived PR branch builds between
-- 9674ebd (main merged on 2026-11-11) and c40d58e (migration 078 became a
-- reserved no-op on 2026-11-13). Preserve the 078 version because those
-- builds may have recorded it; dropping an absent index is safe on clean DBs.
ALTER TABLE server_groups DROP CONSTRAINT IF EXISTS uq_server_groups_project_name;
DROP INDEX IF EXISTS uq_server_groups_project_name;
